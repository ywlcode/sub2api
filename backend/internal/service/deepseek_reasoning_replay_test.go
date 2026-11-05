package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDeepSeekReasoningReplayAcrossProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, source := range []string{"ark", "native"} {
		for _, inbound := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
			for _, destination := range []string{"ark_responses", "native_responses", "native_messages"} {
				t.Run(source+inbound+"/"+destination, func(t *testing.T) {
					body := deepSeekReasoningContinuation(t, source, inbound)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, inbound, bytes.NewReader(body))
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")),
					}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
					account := volcengineTestAccount()
					if destination != "ark_responses" {
						account.Credentials[DeepSeekVolcengineCompatibilityCredential] = false
						account.Credentials["base_url"] = "https://api.deepseek.com"
					}
					if destination == "native_messages" {
						account.Credentials["api_protocol"] = APIProtocolAnthropic
						account.Credentials["base_url"] = "https://api.deepseek.com/anthropic"
					}
					var err error
					switch inbound {
					case "/v1/responses":
						_, err = svc.Forward(context.Background(), c, account, body)
					case "/v1/chat/completions":
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					case "/v1/messages":
						_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
					}
					require.Error(t, err) // Stop after capture; no paid inference.
					require.NotNil(t, upstream.lastReq)
					wire := upstream.lastBody
					require.NotContains(t, string(wire), "<thinking>")
					if destination == "native_messages" {
						require.Equal(t, "/anthropic/v1/messages", upstream.lastReq.URL.Path)
						require.Equal(t, "high", gjson.GetBytes(wire, "output_config.effort").String())
						var thought, callID, resultID string
						for _, message := range gjson.GetBytes(wire, "messages").Array() {
							for _, block := range message.Get("content").Array() {
								switch block.Get("type").String() {
								case "thinking":
									thought += block.Get("thinking").String()
								case "tool_use":
									callID = block.Get("id").String()
								case "tool_result":
									resultID = block.Get("tool_use_id").String()
								}
							}
						}
						require.Equal(t, "original plan", thought)
						require.Equal(t, "call_1", callID)
						require.Equal(t, callID, resultID)
					} else {
						require.Equal(t, "high", gjson.GetBytes(wire, "reasoning.effort").String())
						reasoning := gjson.GetBytes(wire, "input.#(type==\"reasoning\")")
						if destination == "ark_responses" {
							require.Equal(t, "original plan", reasoning.Get("summary.0.text").String())
							require.Equal(t, "summary_text", reasoning.Get("summary.0.type").String())
							require.False(t, reasoning.Get("content").Exists())
						} else {
							require.Equal(t, "original plan", reasoning.Get("content.0.text").String())
							require.Equal(t, "reasoning_text", reasoning.Get("content.0.type").String())
						}
						require.Equal(t, "call_1", gjson.GetBytes(wire, "input.#(type==\"function_call\").call_id").String())
						require.Equal(t, "call_1", gjson.GetBytes(wire, "input.#(type==\"function_call_output\").call_id").String())
						require.Equal(t, "result", gjson.GetBytes(wire, "input.#(type==\"function_call_output\").output").String())
					}
				})
			}
		}
	}
}

// Model a real tool continuation: convert the preceding provider response into
// the client's protocol, then return that assistant message plus its tool result.
func deepSeekReasoningContinuation(t *testing.T, source, inbound string) []byte {
	t.Helper()
	item := apicompat.ResponsesOutput{Type: "reasoning", ID: "rs_fixture"}
	if source == "ark" {
		item.Summary = []apicompat.ResponsesSummary{{Type: "summary_text", Text: "original plan"}}
	} else {
		item.Content = []apicompat.ResponsesContentPart{{Type: "reasoning_text", Text: "original plan"}}
	}
	response := &apicompat.ResponsesResponse{ID: "resp_fixture", Status: "completed", Output: []apicompat.ResponsesOutput{
		item, {Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: "{}"},
	}}
	payload := map[string]any{"model": "deepseek-flash"}
	switch inbound {
	case "/v1/responses":
		payload["tools"] = []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
		payload["reasoning"] = map[string]any{"effort": "high"}
		payload["input"] = []any{
			map[string]any{"type": "message", "role": "user", "content": "question"},
			item, response.Output[1],
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"},
		}
	case "/v1/chat/completions":
		payload["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
		converted := apicompat.ResponsesToChatCompletions(response, "deepseek-flash")
		payload["reasoning_effort"] = "high"
		payload["messages"] = []any{
			map[string]any{"role": "user", "content": "question"},
			converted.Choices[0].Message,
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"},
		}
	case "/v1/messages":
		payload["tools"] = []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}
		converted := apicompat.ResponsesToAnthropic(response, "deepseek-flash")
		payload["max_tokens"] = 1000
		payload["output_config"] = map[string]any{"effort": "high"}
		payload["messages"] = []any{
			map[string]any{"role": "user", "content": "question"},
			map[string]any{"role": "assistant", "content": converted.Content},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "result"}}},
		}
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	return body
}

func TestDeepSeekReasoningReplayDoesNotInventMissingHistory(t *testing.T) {
	account := volcengineTestAccount()
	account.Credentials[DeepSeekVolcengineCompatibilityCredential] = false
	account.Credentials["base_url"] = "https://api.deepseek.com"
	for _, input := range []string{
		"[{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[]}]",
		"[{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"abbreviated\"}]}]",
		"[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}]",
	} {
		body := []byte("{\"input\":" + input + "}")
		got := normalizeDeepSeekResponsesRequestBody(account, body)
		require.JSONEq(t, input, gjson.GetBytes(got, "input").Raw)
	}
	body := []byte("{\"input\":[{\"type\":\"reasoning\",\"content\":[{\"type\":\"reasoning_text\",\"text\":\"full original\"}],\"summary\":[{\"type\":\"summary_text\",\"text\":\"abbreviated\"}]}]}")
	got := normalizeDeepSeekResponsesRequestBody(account, body)
	require.Equal(t, "full original", gjson.GetBytes(got, "input.0.content.0.text").String())
	other := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, body, normalizeDeepSeekResponsesRequestBody(other, body))
}

func TestDeepSeekReasoningReplayUsesSelectedEndpoint(t *testing.T) {
	body := []byte("{\"input\":[{\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"plan\"}]}]}")
	for _, tc := range []struct {
		name, baseURL, responsesURL string
		wantNative                  bool
	}{
		{"official", "https://api.deepseek.com", "", true},
		{"unconfigured_ark", "https://ark.cn-beijing.volces.com/api/v3", "", false},
		{"unknown_relay", "https://relay.example.com", "", false},
		{"adaptive_official", "https://relay.example.com", "https://api.deepseek.com", true},
		{"adaptive_ark", "https://api.deepseek.com", "https://ark.cn-beijing.volces.com/api/v3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := volcengineTestAccount()
			account.Credentials[DeepSeekVolcengineCompatibilityCredential] = false
			account.Credentials["base_url"] = tc.baseURL
			if tc.responsesURL != "" {
				account.Credentials["api_protocol"] = APIProtocolAdaptive
				account.Credentials["api_base_urls"] = map[string]any{APIProtocolResponses: tc.responsesURL}
			}
			got := normalizeDeepSeekResponsesRequestBody(account, body)
			require.Equal(t, tc.wantNative, gjson.GetBytes(got, "input.0.content").Exists())
			require.Equal(t, !tc.wantNative, gjson.GetBytes(got, "input.0.summary").Exists())
		})
	}
}
