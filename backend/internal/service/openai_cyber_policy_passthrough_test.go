//go:build unit

package service

import (
	"bytes"
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
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const cyberPolicyRuleMessage = "cyber-security 内容审计命中风险规则"

// Keep the broad overload rule first, as in production. A transported HTTP 400
// cyber rejection must still match the status+keyword policy rule below it.
func bindCyberPolicyProductionRules(c *gin.Context) *model.ErrorPassthroughRule {
	overloadCode := http.StatusBadGateway
	overloadMessage := "Our servers are currently overloaded. Please try again later."
	cyberMessage := cyberPolicyRuleMessage
	cyber := &model.ErrorPassthroughRule{
		ID: 7, Enabled: true, Platforms: []string{PlatformOpenAI},
		ErrorCodes: []int{http.StatusForbidden}, Keywords: []string{"cyber"}, MatchMode: model.MatchModeAll,
		PassthroughCode: true, CustomMessage: &cyberMessage,
	}
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{
		{
			ID: 5, Enabled: true, Platforms: []string{PlatformOpenAI},
			ErrorCodes: []int{400, 401, 429, 502, 503}, MatchMode: model.MatchModeAny,
			ResponseCode: &overloadCode, CustomMessage: &overloadMessage,
		},
		cyber,
	})
	BindErrorPassthroughService(c, svc)
	return cyber
}

func requireCyberRuleResponse(t *testing.T, c *gin.Context, rec *httptest.ResponseRecorder, err error, upstreamStatus int, originalMessage string) {
	t.Helper()
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "rewriting a cyber rejection must not retry another account")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.True(t, gjson.Valid(rec.Body.String()), "a pre-output error must not flush staged SSE metadata into its JSON body")
	require.Equal(t, cyberPolicyRuleMessage, gjson.Get(rec.Body.String(), "error.message").String())
	mark := GetOpsCyberPolicy(c)
	require.NotNil(t, mark)
	require.Equal(t, upstreamStatus, mark.UpstreamStatus)
	require.Equal(t, originalMessage, mark.Message)
	require.Contains(t, mark.Body, originalMessage)
	require.NotContains(t, mark.Body, cyberPolicyRuleMessage)
}

func TestCyberPolicyHTTPPassthroughProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const original = "flagged by upstream policy"
	body := []byte(`{"error":{"type":"invalid_request_error","code":"cyber_policy","message":"` + original + `","param":"input"}}`)
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		for _, path := range []string{"native", "passthrough", "chat", "messages"} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				bindCyberPolicyProductionRules(c)
				resp := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": 2, "pool_mode_retry_status_codes": []any{400, 403}}}
				svc := &OpenAIGatewayService{}
				var err error
				switch path {
				case "native":
					_, err = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
				case "passthrough":
					err = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, body)
				case "chat":
					_, err = svc.handleCompatErrorResponse(resp, c, account, writeChatCompletionsError)
				case "messages":
					_, err = svc.handleCompatErrorResponse(resp, c, account, writeAnthropicError)
				}
				requireCyberRuleResponse(t, c, rec, err, status, original)
				if path == "native" || path == "passthrough" {
					require.Equal(t, "cyber_policy", gjson.Get(rec.Body.String(), "error.code").String())
					require.Equal(t, "input", gjson.Get(rec.Body.String(), "error.param").String())
				}
			})
		}
	}
}

func TestCyberPolicyResponsesSSEPassthroughProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bare := `{"type":"error","error":{"code":"cyber_policy","message":"original policy rejection"},"usage":{"input_tokens":9,"output_tokens":2}}`
	failed := `{"type":"response.failed","response":{"id":"resp_cyber","status":"failed","error":{"code":"cyber_policy","message":"original policy rejection"},"usage":{"input_tokens":9,"output_tokens":2}}}`
	for _, path := range []string{"native", "passthrough"} {
		for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			for _, terminal := range []string{"failed", "bare", "pair"} {
				for _, started := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/started=%t", path, accountType, terminal, started), func(t *testing.T) {
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
						bindCyberPolicyProductionRules(c)
						stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cyber\",\"status\":\"in_progress\"}}\n\n"
						if started {
							stream += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
						}
						if terminal != "failed" {
							stream += "event: error\ndata: " + bare + "\n\n"
						}
						if terminal != "bare" {
							stream += "event: response.failed\ndata: " + failed + "\n\n"
						}
						resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
						svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, toolCorrector: NewCodexToolCorrector()}
						account := &Account{ID: 1, Platform: PlatformOpenAI, Type: accountType}
						var err error
						if path == "native" {
							_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
						} else {
							_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
						}
						if started {
							require.Equal(t, http.StatusOK, rec.Code)
							require.Contains(t, rec.Body.String(), "hello")
							require.Contains(t, rec.Body.String(), cyberPolicyRuleMessage)
							require.Contains(t, rec.Body.String(), "cyber_policy")
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
	}
}

func TestCyberPolicyBufferedResponsesPassthroughProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough", "raw_chat"} {
		for _, format := range []string{"json", "sse"} {
			if path == "raw_chat" && format == "sse" {
				continue
			}
			t.Run(path+"/"+format, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				bindCyberPolicyProductionRules(c)
				contentType := "application/json"
				body := `{"error":{"code":"cyber_policy","message":"original policy rejection"}}`
				if format == "sse" {
					contentType = "text/event-stream"
					body = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"cyber_policy\",\"message\":\"original policy rejection\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
				svc := &OpenAIGatewayService{}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				var err error
				switch path {
				case "native":
					_, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-5", "gpt-5")
				case "passthrough":
					_, err = svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "gpt-5", "gpt-5")
				case "raw_chat":
					_, err = svc.bufferRawChatCompletions(c, resp, account, "gpt-5", "gpt-5", "gpt-5", nil, nil, time.Now())
				}
				requireCyberRuleResponse(t, c, rec, err, 200, "original policy rejection")
			})
		}
	}
}

func TestCyberPolicyCompatPassthroughProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, committed := range []bool{false, true} {
				if committed && !stream {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%t/committed=%t", path, stream, committed), func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream))
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+path, bytes.NewReader(body))
					bindCyberPolicyProductionRules(c)
					if committed {
						c.Header("Content-Type", "text/event-stream")
						c.Writer.WriteHeaderNow()
					}
					upstream := compatCyberUpstreamRecorder()
					if stream && !committed {
						// Anthropic message_start normally commits HTTP 200 on
						// response.created. Test a rejection before that event too.
						_, failedOnly, _ := strings.Cut(compatCyberUpstreamSSE(), "\n\n")
						upstream.resp.Body = io.NopCloser(strings.NewReader(failedOnly))
					}
					svc := &OpenAIGatewayService{httpUpstream: upstream}
					var result *OpenAIForwardResult
					var err error
					if path == "chat" {
						result, err = svc.ForwardAsChatCompletions(context.Background(), c, compatCyberOAuthAccount(), body, "", "gpt-5.5")
					} else {
						result, err = svc.ForwardAsAnthropic(context.Background(), c, compatCyberOAuthAccount(), body, "", "gpt-5.5")
					}
					require.Nil(t, result, "cyber must not enter normal billing")
					if committed {
						require.Error(t, err)
						require.Equal(t, http.StatusOK, rec.Code)
						require.Contains(t, rec.Body.String(), cyberPolicyRuleMessage)
						if path == "chat" {
							require.Contains(t, rec.Body.String(), "data: [DONE]")
						} else {
							require.Contains(t, rec.Body.String(), "event: error")
						}
					} else {
						requireCyberRuleResponse(t, c, rec, err, 200, "flagged for cyber policy")
					}
				})
			}
		}
	}
}

func TestCyberPolicyRuleOptionsAndUnmatchedPreservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"custom_status", "original_message", "disabled", "wrong_platform", "session_blocked"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			rule := bindCyberPolicyProductionRules(c)
			account := &Account{Platform: PlatformOpenAI}
			payload := []byte(`{"type":"error","status":400,"error":{"code":"cyber_policy","message":"original message"}}`)
			switch mode {
			case "custom_status":
				code := http.StatusUnavailableForLegalReasons
				rule.PassthroughCode = false
				rule.ResponseCode = &code
				rule.SkipMonitoring = true
			case "original_message":
				rule.PassthroughBody = true
			case "disabled":
				rule.Enabled = false
			case "wrong_platform":
				account.Platform = PlatformGrok
			case "session_blocked":
				payload = []byte(`{"error":{"code":"session_blocked_by_cyber_policy","message":"blocked"}}`)
			}
			original := append([]byte(nil), payload...)
			client, status, matched := rewriteOpenAICyberPolicyForClient(c, account, payload)
			require.Equal(t, original, payload, "the evidence buffer must not change")
			switch mode {
			case "custom_status":
				require.True(t, matched)
				require.Equal(t, http.StatusUnavailableForLegalReasons, status)
				require.Equal(t, int64(status), gjson.GetBytes(client, "status").Int())
				require.True(t, c.GetBool(OpsSkipPassthroughKey))
			case "original_message":
				require.True(t, matched)
				require.Equal(t, "original message", gjson.GetBytes(client, "error.message").String())
			default:
				require.False(t, matched)
				require.Equal(t, original, client)
			}
		})
	}
}

func TestCyberPolicyRawChatStreamProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			bindCyberPolicyProductionRules(c)
			if committed {
				c.Header("Content-Type", "text/event-stream")
				c.Writer.WriteHeaderNow()
			}
			body := "data: {\"error\":{\"code\":\"cyber_policy\",\"message\":\"original policy rejection\"},\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			_, err := (&OpenAIGatewayService{}).streamRawChatCompletions(c, resp, &Account{Platform: PlatformOpenAI}, "gpt-5", "gpt-5", "gpt-5", nil, nil, time.Now(), 10)
			if committed {
				require.ErrorIs(t, err, errOpenAICyberPolicyForwarded)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), cyberPolicyRuleMessage)
				require.Contains(t, rec.Body.String(), "data: [DONE]")
			} else {
				requireCyberRuleResponse(t, c, rec, err, 200, "original policy rejection")
			}
		})
	}
}

func TestCyberPolicyChatUpstreamFallbackProductionRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, committed := range []bool{false, true} {
				if committed && !stream {
					continue
				}
				t.Run(fmt.Sprintf("%s/stream=%t/committed=%t", path, stream, committed), func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					request := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":"hi","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream))
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+path, bytes.NewReader(request))
					bindCyberPolicyProductionRules(c)
					if committed {
						c.Header("Content-Type", "text/event-stream")
						c.Writer.WriteHeaderNow()
					}
					body := `{"error":{"code":"cyber_policy","message":"original policy rejection"},"usage":{"prompt_tokens":9,"completion_tokens":2}}`
					if stream {
						body = "data: " + body + "\n\n"
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
					var result *OpenAIForwardResult
					var err error
					if path == "responses" {
						result, err = svc.forwardResponsesViaRawChatCompletions(context.Background(), c, forceChatResponsesFallbackAccount(), request, time.Now())
					} else {
						result, err = svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), request, "", "")
					}
					require.Nil(t, result, "a mapped cyber error must not be converted to an empty successful response")
					if committed {
						require.Error(t, err)
						require.Equal(t, http.StatusOK, rec.Code)
						require.Contains(t, rec.Body.String(), cyberPolicyRuleMessage)
						if path == "responses" {
							require.Contains(t, rec.Body.String(), "event: response.failed")
						} else {
							require.Contains(t, rec.Body.String(), "event: error")
						}
					} else {
						requireCyberRuleResponse(t, c, rec, err, 200, "original policy rejection")
					}
					require.Equal(t, 9, GetOpsCyberPolicy(c).UpstreamInTok)
					require.Equal(t, 2, GetOpsCyberPolicy(c).UpstreamOutTok)
				})
			}
		}
	}
}
