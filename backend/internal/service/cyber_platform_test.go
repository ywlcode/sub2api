//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cyberevidence"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCyberFixedEnvelopeNeverLeaksVendorFieldsOrMatchesRules(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformMiniMax} {
		for _, code := range []string{"cyber_policy", "session_blocked_by_cyber_policy"} {
			for _, envelope := range []string{"http", "error", "response.failed"} {
				t.Run(platform+"/"+code+"/"+envelope, func(t *testing.T) {
					payload := `{"error":{"code":"` + code + `","type":"vendor-secret","message":"vendor-secret","param":"vendor-secret"},"output":["vendor-secret"],"metadata":{"secret":"vendor-secret"}}`
					if envelope == "response.failed" {
						payload = `{"type":"response.failed","response":` + payload + `,"vendor_extension":"vendor-secret"}`
					} else if envelope == "error" {
						payload = `{"type":"error",` + payload[1:]
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					rule := bindCyberPolicyProductionRules(c)
					rule.PassthroughBody, rule.SkipMonitoring = true, true
					original := []byte(payload)
					client, status, changed := rewriteOpenAIForbiddenErrorForClient(c, &Account{Platform: platform}, original, 400)
					require.True(t, changed)
					require.Equal(t, 403, status)
					require.Equal(t, payload, string(original))
					require.Contains(t, string(client), CyberPolicyClientMessage)
					require.NotContains(t, string(client), "vendor-secret")
					require.NotContains(t, string(client), `"code":"cyber_policy"`)
					require.False(t, c.GetBool(OpsSkipPassthroughKey))
					if envelope == "response.failed" {
						require.Equal(t, "response.failed", gjson.GetBytes(client, "type").String())
						require.Equal(t, "failed", gjson.GetBytes(client, "response.status").String())
					}
				})
			}
		}
	}
}

func TestCyberEvidenceRetainedForEveryPlatform(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek, PlatformGrok, PlatformAnthropic, PlatformGemini, PlatformKimi, PlatformZhipu, PlatformMiniMax} {
		t.Run(platform, func(t *testing.T) {
			store := &cyberEvidenceRecordStore{}
			evidence := cyberevidence.New(store, reauthTestEncryptor{}, true)
			defer evidence.Close()
			repo := &cyberEvidenceCreateLogRepo{}
			svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true"}}, repo, nil, nil, nil, nil, nil, nil).WithCyberEvidence(evidence)
			prepared := evidence.PrepareWithSession([]byte(`{"input":"synthetic fixture"}`), "original upstream policy", "session-1")
			defer prepared.Release()
			svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{Platform: platform, Evidence: prepared, UserID: 42, Model: "alias", UpstreamMessage: "original upstream policy"})
			require.Eventually(t, func() bool { return store.snapshotSaves() == 1 }, time.Second, time.Millisecond)
			logs := repo.snapshotLogs()
			require.Len(t, logs, 1)
			require.Equal(t, platform, logs[0].Provider)
			require.Equal(t, "pending", logs[0].EvidenceStatus)
			require.Contains(t, logs[0].Error, "original upstream policy")
			if platform != PlatformOpenAI {
				require.Equal(t, ContentModerationModeCyberLogOnly, logs[0].Mode)
			}
		})
	}
}

func TestCyberNativeAnthropicUpstreamAllClientProtocols(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic} {
		for _, stream := range []bool{false, true} {
			for _, status := range []int{200, 400} {
				t.Run(fmt.Sprintf("%s/stream=%t/status=%d", protocol, stream, status), func(t *testing.T) {
					request := []byte(fmt.Sprintf(`{"model":"test-model","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":128,"stream":%t}`, stream))
					payload := `{"type":"error","error":{"code":"cyber_policy","message":"original native policy","vendor_field":"hidden"}}`
					header := http.Header{"Content-Type": {"application/json"}}
					// Converted clients always consume Anthropic SSE, even when buffered.
					if status == 200 && (stream || protocol != APIProtocolAnthropic) {
						payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\nevent: error\ndata: " + payload + "\n\n"
						header.Set("Content-Type", "text/event-stream")
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(payload))}}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
					account := &Account{ID: 14, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://example.test", "api_protocol": APIProtocolAnthropic}}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(request))
					bindCyberPolicyProductionRules(c)
					var err error
					switch protocol {
					case APIProtocolResponses:
						_, err = svc.Forward(context.Background(), c, account, request)
					case APIProtocolChatCompletions:
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, request, "", "")
					default:
						_, err = svc.ForwardAsAnthropic(context.Background(), c, account, request, "", "")
					}
					require.Error(t, err)
					require.Len(t, upstream.requests, 1)
					require.NotNil(t, GetOpsCyberPolicy(c))
					require.Equal(t, "original native policy", GetOpsCyberPolicy(c).Message)
					require.Contains(t, rec.Body.String(), CyberPolicyClientMessage)
					require.NotContains(t, rec.Body.String(), "original native policy")
					require.NotContains(t, rec.Body.String(), "hidden")
				})
			}
		}
	}
}

func TestCyberVolcengineCompactionRetainsEvidenceWithoutRetry(t *testing.T) {
	for _, status := range []int{200, 400} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", status, stream), func(t *testing.T) {
				body := volcengineCompactionTestBody(t, stream)
				payload := `{"error":{"code":"cyber_policy","message":"original compaction policy"}}`
				header := http.Header{"Content-Type": {"application/json"}}
				if status == 200 && stream {
					payload = "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"cyber_policy\",\"message\":\"original compaction policy\"}}\n\n"
					header.Set("Content-Type", "text/event-stream")
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(payload))}}
				req, reqErr := http.NewRequest(http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
				require.NoError(t, reqErr)
				_, err := accountVolcengineHTTP(upstream, volcengineTestAccount()).Do(req, "", 144, 1)
				require.Error(t, err)
				require.Len(t, upstream.requests, 1)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = req
				_, handled := writeVolcengineCompactionError(c, err, stream, "ep-ark")
				require.True(t, handled)
				require.NotNil(t, GetOpsCyberPolicy(c))
				require.Equal(t, "original compaction policy", GetOpsCyberPolicy(c).Message)
				require.Contains(t, rec.Body.String(), CyberPolicyClientMessage)
				require.NotContains(t, rec.Body.String(), "original compaction policy")
			})
		}
	}
}
