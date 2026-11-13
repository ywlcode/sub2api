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
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var forbiddenPassthroughCases = []struct{ name, errorJSON string }{
	{"session", `{"code":"session_blocked_by_cyber_policy","message":"该会话已被网络安全策略屏蔽，请开启新会话"}`},
	{"permission", `{"code":"content_blocked","type":"permission_error","message":"original denial"}`},
	{"explicit_status", `{"code":"provider_rejection","type":"invalid_request_error","status_code":403,"message":"original denial"}`},
}

func forbiddenTestMessage(name string) string {
	if name == "session" {
		return CyberPolicyClientMessage
	}
	return cyberPolicyRuleMessage
}

func bindForbiddenTestRule(c *gin.Context, name string) {
	c.Set("test.forbidden_message", forbiddenTestMessage(name))
	rule := bindCyberPolicyProductionRules(c)
	if name != "session" {
		// An ordinary 403 must match even without any cyber-related keyword.
		rule.Keywords = nil
		getBoundErrorPassthroughService(c).setLocalCache([]*model.ErrorPassthroughRule{rule})
	}
}

func requireForbiddenClientError(t *testing.T, c *gin.Context, rec *httptest.ResponseRecorder, err error, streaming bool) {
	t.Helper()
	require.NotEmpty(t, c.GetString("test.forbidden_message"))
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.Nil(t, GetOpsCyberPolicy(c), "client rule matching must not create a new cyber violation")
	require.Contains(t, rec.Body.String(), c.GetString("test.forbidden_message"))
	if streaming {
		require.Equal(t, http.StatusOK, rec.Code)
	} else {
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.True(t, gjson.Valid(rec.Body.String()), "staged SSE metadata must not leak into a JSON error")
		require.Equal(t, c.GetString("test.forbidden_message"), gjson.Get(rec.Body.String(), "error.message").String())
	}
}

func TestForbiddenHTTPPassthroughAllErrorBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range forbiddenPassthroughCases {
		for _, path := range []string{"native", "passthrough", "chat", "messages"} {
			for _, status := range []int{403, 502} {
				if status == 502 && tc.name != "session" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%d", tc.name, path, status), func(t *testing.T) {
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					bindForbiddenTestRule(c, tc.name)
					body := `{"error":` + tc.errorJSON + `}`
					resp := newOpenAIUpstreamErrorResponse(status, body)
					h := newOpenAI403TestHarness(t, 144, 1)
					svc, account := &OpenAIGatewayService{rateLimitService: h.svc}, h.account
					var err error
					switch path {
					case "native":
						_, err = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
					case "passthrough":
						err = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, []byte(body))
					case "chat":
						_, err = svc.handleCompatErrorResponse(resp, c, account, writeChatCompletionsError)
					case "messages":
						_, err = svc.handleCompatErrorResponse(resp, c, account, writeAnthropicError)
					}
					requireForbiddenClientError(t, c, rec, err, false)
					if tc.name == "session" {
						h.requireNoAccountPenalty(t)
						require.False(t, shouldFailoverOpenAIPassthroughResponse(account, status, []byte(body)))
						require.Contains(t, c.GetString(OpsUpstreamErrorMessageKey), "开启新会话")
						if path == "native" || path == "passthrough" {
							require.Equal(t, "session_blocked_by_cyber_policy", gjson.Get(rec.Body.String(), "error.code").String())
						}
					}
				})
			}
		}
	}
}

func TestForbiddenResponsesSSEPassthrough(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, path := range []string{"native", "passthrough"} {
			for _, terminal := range []string{"failed", "bare", "pair"} {
				for _, started := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/started=%t", tc.name, path, terminal, started), func(t *testing.T) {
						c, rec := newOpenAIUpstreamErrorTestContext(t)
						bindForbiddenTestRule(c, tc.name)
						stream := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_forbidden\"}}\n\n"
						if started {
							stream += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
						}
						if terminal != "failed" {
							stream += "event: error\ndata: {\"type\":\"error\",\"error\":" + tc.errorJSON + "}\n\n"
						}
						if terminal != "bare" {
							stream += "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_forbidden\",\"status\":\"failed\",\"error\":" + tc.errorJSON + "}}\n\n"
						}
						resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
						svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
						account := &Account{ID: 144, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
						var err error
						if path == "native" {
							_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
						} else {
							_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
						}
						requireForbiddenClientError(t, c, rec, err, started)
						require.Contains(t, rec.Body.String(), gjson.Get(tc.errorJSON, "code").String())
						require.NotContains(t, rec.Body.String(), gjson.Get(tc.errorJSON, "message").String())
					})
				}
			}
		}
	}
}

func TestForbiddenBufferedResponsesAndRawChat(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, path := range []string{"native", "passthrough", "raw_chat"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/sse=%t", tc.name, path, stream), func(t *testing.T) {
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					bindForbiddenTestRule(c, tc.name)
					body := `{"error":` + tc.errorJSON + `}`
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
						if path == "raw_chat" {
							body = "data: " + body + "\n\n"
						} else {
							body = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":" + tc.errorJSON + "}}\n\n"
						}
					}
					resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
					svc := &OpenAIGatewayService{}
					account := &Account{ID: 144, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
					var err error
					switch path {
					case "native":
						_, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-5", "gpt-5")
					case "passthrough":
						_, err = svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "gpt-5", "gpt-5")
					case "raw_chat":
						if stream {
							_, err = svc.streamRawChatCompletions(c, resp, account, "gpt-5", "gpt-5", "gpt-5", nil, nil, time.Now(), 10)
						} else {
							_, err = svc.bufferRawChatCompletions(c, resp, account, "gpt-5", "gpt-5", "gpt-5", nil, nil, time.Now())
						}
					}
					requireForbiddenClientError(t, c, rec, err, false)
				})
			}
		}
	}
}

func TestForbiddenRuleGuardsAndOptions(t *testing.T) {
	for _, body := range []string{
		`{"status":403,"output":[{"text":"permission_error session_blocked_by_cyber_policy"}]}`,
		`{"response":{"status":"completed","output":[{"error":{"code":"session_blocked_by_cyber_policy"}}]}}`,
		`{"error":{"type":"invalid_request_error","message":"invalid input"}}`,
	} {
		c, _ := newOpenAIUpstreamErrorTestContext(t)
		bindForbiddenTestRule(c, "generic")
		client, _, matched := rewriteOpenAIForbiddenErrorForClient(c, &Account{Platform: PlatformOpenAI}, []byte(body), 200)
		require.False(t, matched)
		require.Equal(t, body, string(client))
	}
	for _, tc := range forbiddenPassthroughCases {
		for _, mode := range []string{"custom_status", "original_message", "disabled", "wrong_platform"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				c, _ := newOpenAIUpstreamErrorTestContext(t)
				rule := bindCyberPolicyProductionRules(c)
				rule.Keywords = nil
				getBoundErrorPassthroughService(c).setLocalCache([]*model.ErrorPassthroughRule{rule})
				account := &Account{Platform: PlatformOpenAI}
				switch mode {
				case "custom_status":
					status := 451
					rule.PassthroughCode, rule.ResponseCode, rule.SkipMonitoring = false, &status, true
				case "original_message":
					rule.PassthroughBody = true
				case "disabled":
					rule.Enabled = false
				case "wrong_platform":
					account.Platform = PlatformGrok
				}
				body := `{"type":"response.failed","response":{"status":"failed","error":` + tc.errorJSON + `}}`
				payload := []byte(body)
				client, status, matched := rewriteOpenAIForbiddenErrorForClient(c, account, payload, 0)
				require.Equal(t, body, string(payload), "audit evidence must not be mutated")
				require.Nil(t, GetOpsCyberPolicy(c))
				if tc.name == "session" {
					require.True(t, matched)
					require.Equal(t, http.StatusForbidden, status)
					require.Equal(t, CyberPolicyClientMessage, gjson.GetBytes(client, "response.error.message").String())
					require.False(t, c.GetBool(OpsSkipPassthroughKey))
					return
				}
				if mode == "disabled" || mode == "wrong_platform" {
					require.False(t, matched)
					require.Equal(t, payload, client)
					return
				}
				require.True(t, matched)
				require.Equal(t, "failed", gjson.GetBytes(client, "response.status").String())
				if mode == "custom_status" {
					require.Equal(t, 451, status)
					require.True(t, c.GetBool(OpsSkipPassthroughKey))
					if tc.name == "explicit_status" {
						require.Equal(t, int64(451), gjson.GetBytes(client, "response.error.status_code").Int())
					}
				} else {
					require.Equal(t, gjson.Get(tc.errorJSON, "message").String(), gjson.GetBytes(client, "response.error.message").String())
				}
			})
		}
	}
}

func TestForbiddenSIWCAndNonJSONPassthrough(t *testing.T) {
	for _, body := range []string{"", "<html>Access denied</html>", `{"error":{"code":"subscription_sharing_user_not_eligible","message":"private upstream message","metadata":"private metadata"}}`} {
		for _, failover := range []bool{false, true} {
			if failover && !strings.Contains(body, siwcNotEligibleCode) {
				continue
			}
			t.Run(fmt.Sprintf("body=%s/failover=%t", body, failover), func(t *testing.T) {
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				bindForbiddenTestRule(c, "generic")
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{OpenAISIWCEnabledKey: true}}
				svc := &OpenAIGatewayService{}
				resp := newOpenAIUpstreamErrorResponse(403, body)
				var err error
				if failover {
					err = svc.handleFailoverErrorResponsePassthrough(context.Background(), resp, c, account, nil, []byte(body))
				} else {
					err = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, []byte(body))
				}
				requireForbiddenClientError(t, c, rec, err, false)
				require.NotContains(t, rec.Body.String(), "private")
			})
		}
	}
}

func TestForbiddenSessionAfterQueueHeartbeat(t *testing.T) {
	for _, path := range []string{"native", "passthrough"} {
		t.Run(path, func(t *testing.T) {
			c, rec, _, _ := newHeaderWaitTest(t, nil)
			bindForbiddenTestRule(c, "session")
			c.Header("Content-Type", "text/event-stream")
			_, err := c.Writer.WriteString(":\n\n")
			require.NoError(t, err)
			recordOpenAIStreamKeepaliveBytes(c, 3)
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 144, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			resp := newOpenAIUpstreamErrorResponse(403, upstreamSessionBlockedBody)
			if path == "native" {
				_, err = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
			} else {
				err = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, []byte(upstreamSessionBlockedBody))
			}
			requireForbiddenClientError(t, c, rec, err, true)
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
			require.Contains(t, rec.Body.String(), "session_blocked_by_cyber_policy")
			require.Contains(t, rec.Body.String(), CyberPolicyClientMessage)
			require.NotContains(t, rec.Body.String(), "该会话已被网络安全策略屏蔽")
			require.Contains(t, c.GetString(OpsUpstreamErrorMessageKey), "开启新会话")
		})
	}
}

func TestForbiddenCompatResponsesEvents(t *testing.T) {
	for _, tc := range forbiddenPassthroughCases {
		for _, path := range []string{"chat", "messages"} {
			for _, mode := range []string{"buffered", "stream", "committed"} {
				t.Run(tc.name+"/"+path+"/"+mode, func(t *testing.T) {
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					bindForbiddenTestRule(c, tc.name)
					request := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, mode != "buffered"))
					if mode == "committed" {
						c.Header("Content-Type", "text/event-stream")
						c.Writer.WriteHeaderNow()
					}
					body := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_forbidden\",\"status\":\"failed\",\"error\":" + tc.errorJSON + "}}\n\n"
					upstream := compatCyberUpstreamRecorder()
					upstream.resp.Body = io.NopCloser(strings.NewReader(body))
					svc := &OpenAIGatewayService{httpUpstream: upstream}
					var err error
					if path == "chat" {
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, compatCyberOAuthAccount(), request, "", "gpt-5.5")
					} else {
						_, err = svc.ForwardAsAnthropic(context.Background(), c, compatCyberOAuthAccount(), request, "", "gpt-5.5")
					}
					requireForbiddenClientError(t, c, rec, err, mode == "committed")
				})
			}
		}
	}
}
