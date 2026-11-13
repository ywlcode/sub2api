//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCyberPolicyHTTPViaWSProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, terminal := range []string{"error", "response.failed"} {
		for _, mode := range []string{"buffered", "stream", "after_output"} {
			t.Run(terminal+"/"+mode, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				bindCyberPolicyProductionRules(c)
				cfg := newOpenAIWSV2TestConfig()
				cfg.Security.URLAllowlist.Enabled = false
				cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
				payload := `{"type":"error","error":{"code":"cyber_policy","type":"rate_limit_error","message":"original policy rejection"},"usage":{"input_tokens":9,"output_tokens":2}}`
				if terminal == "response.failed" {
					payload = `{"type":"response.failed","response":{"id":"resp_cyber","status":"failed","error":{"code":"cyber_policy","message":"original policy rejection"},"usage":{"input_tokens":9,"output_tokens":2}}}`
				}
				conn := &openAIWSCaptureConn{events: [][]byte{[]byte(payload)}}
				if mode == "after_output" {
					conn.events = append([][]byte{[]byte(`{"type":"response.output_text.delta","delta":"hello"}`)}, conn.events...)
				}
				pool := newOpenAIWSConnPool(cfg)
				defer pool.Close()
				pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
				_, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"input":"hi"}`, mode != "buffered")))
				if mode == "after_output" {
					require.Equal(t, http.StatusOK, rec.Code)
					require.Contains(t, rec.Body.String(), CyberPolicyClientMessage)
					require.NotContains(t, rec.Body.String(), "original policy rejection")
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
				} else {
					requireCyberRuleResponse(t, c, rec, err, 200, "original policy rejection")
				}
				mark := GetOpsCyberPolicy(c)
				require.NotNil(t, mark)
				require.Equal(t, "original policy rejection", mark.Message)
				require.Equal(t, 9, mark.UpstreamInTok)
				require.Equal(t, 2, mark.UpstreamOutTok)
			})
		}
	}
}

func TestCyberPolicyNativeWSProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		for _, pair := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pair=%t", mode, pair), func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				upstream := newStagedPassthroughConn()
				if pair {
					upstream.Send(`{"type":"error","error":{"code":"cyber_policy","type":"rate_limit_error","message":"original policy rejection"},"usage":{"input_tokens":5,"output_tokens":1}}`)
				}
				upstream.Send(`{"type":"response.failed","response":{"id":"resp_cyber","status":"failed","error":{"code":"cyber_policy","message":"paired policy rejection"},"usage":{"input_tokens":9,"output_tokens":2}}}`)
				cfg := passthroughLifecycleConfig()
				svc := newPassthroughLifecycleService(cfg, upstream)
				account := passthroughLifecycleAccount()
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				if mode == OpenAIWSIngressModeCtxPool {
					cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
					pool := newOpenAIWSConnPool(cfg)
					defer pool.Close()
					pool.setClientDialerForTest(&stagedPassthroughDialer{conn: upstream})
					svc.openaiWSPool = pool
				}
				marks := make(chan CyberPolicyMark, 2)
				server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
					bindCyberPolicyProductionRules(c)
					return &OpenAIWSIngressHooks{AfterTurn: func(_ int, _ *OpenAIForwardResult, _ error) {
						if mark := GetOpsCyberPolicy(c); mark != nil {
							marks <- *mark
						}
					}}
				})
				defer server.Close()
				client := dialPassthroughLifecycleClient(t, server)
				defer func() { _ = client.CloseNow() }()
				count := 1
				if pair {
					count++
				}
				for range count {
					frame, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err)
					require.Contains(t, string(frame), CyberPolicyClientMessage)
					require.Contains(t, string(frame), "cyber_policy")
					require.NotContains(t, string(frame), "policy rejection")
				}
				select {
				case mark := <-marks:
					require.Equal(t, http.StatusOK, mark.UpstreamStatus)
					if pair {
						require.Equal(t, "original policy rejection", mark.Message)
						require.Equal(t, 5, mark.UpstreamInTok)
						require.Equal(t, 1, mark.UpstreamOutTok)
					} else {
						require.Equal(t, "paired policy rejection", mark.Message)
						require.Equal(t, 9, mark.UpstreamInTok)
						require.Equal(t, 2, mark.UpstreamOutTok)
					}
					require.NotContains(t, mark.Body, cyberPolicyRuleMessage)
				case <-time.After(3 * time.Second):
					t.Fatal("cyber evidence was not delivered to AfterTurn")
				}
				_ = client.Close(coderws.StatusNormalClosure, "done")
				select {
				case err := <-serverErr:
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
				case <-time.After(3 * time.Second):
					t.Fatal("WS did not finish")
				}
			})
		}
	}
}

func TestCyberPolicyWSHTTPBridgeProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, format := range []string{"http", "error", "response.failed", "oauth_bare_eof"} {
		t.Run(format, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			bindCyberPolicyProductionRules(c)
			status := http.StatusOK
			body := `{"type":"error","error":{"code":"cyber_policy","message":"original policy rejection"},"usage":{"input_tokens":9,"output_tokens":2}}`
			switch format {
			case "http":
				status = http.StatusBadRequest
			case "response.failed":
				body = "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_cyber\",\"status\":\"failed\",\"error\":{\"code\":\"cyber_policy\",\"message\":\"original policy rejection\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
			default:
				body = "data: " + body + "\n\n"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 1, Status: StatusActive, Schedulable: true, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
			if format == "oauth_bare_eof" {
				account = compatCyberOAuthAccount()
			}
			var clientFrames [][]byte
			request := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)
			_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "sk-test", request, len(request), "gpt-5", "", "", "", "", 1, func(frame []byte) error {
				clientFrames = append(clientFrames, append([]byte(nil), frame...))
				return nil
			})
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.NotEmpty(t, clientFrames)
			clientError := clientFrames[len(clientFrames)-1]
			require.True(t, isOpenAIUpstreamSessionBlocked(clientError))
			require.Equal(t, CyberPolicyClientMessage, extractOpenAISSEErrorMessage(clientError))
			if format == "http" {
				require.Equal(t, int64(403), gjson.GetBytes(clientError, "status").Int())
			}
			mark := GetOpsCyberPolicy(c)
			require.NotNil(t, mark)
			require.Equal(t, status, mark.UpstreamStatus)
			require.Equal(t, "original policy rejection", mark.Message)
			require.NotContains(t, mark.Body, cyberPolicyRuleMessage)
		})
	}
}
