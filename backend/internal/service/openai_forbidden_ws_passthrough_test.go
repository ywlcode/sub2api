//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForbiddenHTTPViaWS(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, terminal := range []string{"error", "response.failed"} {
			for _, mode := range []string{"buffered", "stream", "after_output"} {
				t.Run(tc.name+"/"+terminal+"/"+mode, func(t *testing.T) {
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					bindForbiddenTestRule(c, tc.name)
					cfg := newOpenAIWSV2TestConfig()
					cfg.Security.URLAllowlist.Enabled = false
					cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
					payload := `{"type":"error","error":` + tc.errorJSON + `}`
					if terminal == "response.failed" {
						payload = `{"type":"response.failed","response":{"id":"resp_forbidden","status":"failed","error":` + tc.errorJSON + `}}`
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
						require.Equal(t, 200, rec.Code)
						require.Contains(t, rec.Body.String(), cyberPolicyRuleMessage)
					} else {
						requireForbiddenClientError(t, c, rec, err, false)
					}
					require.Nil(t, GetOpsCyberPolicy(c))
					require.Contains(t, rec.Body.String(), gjson.Get(tc.errorJSON, "code").String())
					require.NotContains(t, rec.Body.String(), gjson.Get(tc.errorJSON, "message").String())
				})
			}
		}
	}
}

func TestForbiddenNativeWS(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				upstream := newStagedPassthroughConn()
				upstream.Send(`{"type":"error","error":` + tc.errorJSON + `}`)
				upstream.Send(`{"type":"response.failed","response":{"id":"resp_forbidden","status":"failed","error":` + tc.errorJSON + `}}`)
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
				marks := make(chan *CyberPolicyMark, 1)
				server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
					bindForbiddenTestRule(c, tc.name)
					return &OpenAIWSIngressHooks{AfterTurn: func(_ int, _ *OpenAIForwardResult, _ error) { marks <- GetOpsCyberPolicy(c) }}
				})
				defer server.Close()
				client := dialPassthroughLifecycleClient(t, server)
				defer func() { _ = client.CloseNow() }()
				for range 2 {
					frame, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err)
					require.Contains(t, string(frame), cyberPolicyRuleMessage)
					require.Contains(t, string(frame), gjson.Get(tc.errorJSON, "code").String())
				}
				select {
				case mark := <-marks:
					require.Nil(t, mark)
				case <-time.After(3 * time.Second):
					t.Fatal("turn did not complete")
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

func TestForbiddenWSHTTPBridge(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, format := range []string{"http", "error", "response.failed"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				c, _ := newOpenAIUpstreamErrorTestContext(t)
				bindForbiddenTestRule(c, tc.name)
				status := 200
				body := `{"type":"error","error":` + tc.errorJSON + `}`
				switch format {
				case "http":
					status = 403
				case "response.failed":
					body = "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_forbidden\",\"status\":\"failed\",\"error\":" + tc.errorJSON + "}}\n\n"
				default:
					body = "data: " + body + "\n\n"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 144, Status: StatusActive, Schedulable: true, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}
				var frames [][]byte
				request := []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)
				turn := 1
				if tc.name != "session" && format == "http" {
					// Ordinary HTTP 403 retains first-turn failover; later turns
					// cannot switch accounts and must apply the rule here.
					turn = 2
				}
				_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "sk-test", request, len(request), "gpt-5", "", "", "", "", turn, func(frame []byte) error {
					frames = append(frames, append([]byte(nil), frame...))
					return nil
				})
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
				require.NotEmpty(t, frames)
				require.Contains(t, string(frames[len(frames)-1]), cyberPolicyRuleMessage)
				require.Contains(t, string(frames[len(frames)-1]), gjson.Get(tc.errorJSON, "code").String())
				require.Nil(t, GetOpsCyberPolicy(c))
			})
		}
	}
}

func TestForbiddenWSDialRulesPreserveEvidence(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newOpenAIUpstreamErrorTestContext(t)
			bindForbiddenTestRule(c, tc.name)
			body := `{"error":` + tc.errorJSON + `}`
			upstream := &openAIWSDialError{StatusCode: 403, ResponseBody: []byte(body), Err: errors.New("handshake rejected")}
			var frame []byte
			err, matched := forwardOpenAIForbiddenWSDialError(c, &Account{Platform: PlatformOpenAI}, upstream, func(event []byte) error {
				frame = append([]byte(nil), event...)
				return nil
			})
			require.True(t, matched)
			require.ErrorIs(t, err, upstream)
			require.Equal(t, "error", gjson.GetBytes(frame, "type").String())
			require.Equal(t, cyberPolicyRuleMessage, gjson.GetBytes(frame, "error.message").String())
			require.Equal(t, gjson.Get(tc.errorJSON, "code").String(), gjson.GetBytes(frame, "error.code").String())
			require.Equal(t, body, string(upstream.ResponseBody))
			require.Nil(t, GetOpsCyberPolicy(c))
		})
	}
}

func TestForbiddenHTTPViaWSHandshakeFallback(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", tc.name, committed), func(t *testing.T) {
				c, rec, _, _ := newHeaderWaitTest(t, nil)
				bindForbiddenTestRule(c, tc.name)
				if committed {
					c.Header("Content-Type", "text/event-stream")
					_, err := c.Writer.WriteString(":\n\n")
					require.NoError(t, err)
				}
				body := `{"error":` + tc.errorJSON + `}`
				err := wrapOpenAIWSFallback("handshake_failed", &openAIWSDialError{StatusCode: 403, ResponseBody: []byte(body), Err: errors.New("handshake rejected")})
				svc := &OpenAIGatewayService{}
				account := &Account{Platform: PlatformOpenAI}
				require.True(t, svc.writeOpenAIWSFallbackErrorResponse(c, account, err))
				requireForbiddenClientError(t, c, rec, err, committed)
				require.Contains(t, rec.Body.String(), gjson.Get(tc.errorJSON, "code").String())
				before := rec.Body.String()
				require.False(t, svc.writeOpenAIWSFallbackErrorResponse(c, account, err))
				require.Equal(t, before, rec.Body.String(), "the final error must only be written once")
			})
		}
	}
}
