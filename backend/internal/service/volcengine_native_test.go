package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Native fixtures include a signed thought and parallel tool calls. The next
// request replays the upstream blocks themselves; no bridge reconstructs them.
func TestVolcengineNativeProtocolsReplayHistory(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic} {
		for _, stream := range []bool{false, true} {
			for _, effort := range []string{"none", "high", "max"} {
				t.Run(fmt.Sprintf("%s/%t/%s", protocol, stream, effort), func(t *testing.T) {
					account := volcengineTestAccount()
					account.Credentials["api_protocol"] = APIProtocolResponses // legacy stored setting
					account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
					thought := `{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"original plan"}]}`
					blocks := `[{"type":"thinking","thinking":"original plan","signature":"native-signature"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"n":1}},{"type":"tool_use","id":"call_2","name":"lookup","input":{"n":2}}]`
					chat := `{"role":"assistant","reasoning_content":"original plan","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"n\":1}"}},{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"n\":2}"}}]}`
					if effort == "none" {
						thought = ""
						blocks = `[{"type":"tool_use","id":"call_1","name":"lookup","input":{"n":1}},{"type":"tool_use","id":"call_2","name":"lookup","input":{"n":2}}]`
						chat = strings.Replace(chat, `"reasoning_content":"original plan",`, "", 1)
					}
					calls := `{"type":"function_call","id":"fc1","call_id":"call_1","name":"lookup","arguments":"{\"n\":1}"},{"type":"function_call","id":"fc2","call_id":"call_2","name":"lookup","arguments":"{\"n\":2}"}`
					output := "[" + calls + "]"
					if thought != "" {
						output = "[" + thought + "," + calls + "]"
					}
					path, target := "/v1/responses", "/api/v3/responses"
					request := map[string]any{"model": "ep-ark", "stream": stream, "input": "hello", "reasoning": map[string]any{"effort": effort}}
					response := `{"id":"resp_1","object":"response","model":"ep-ark","status":"completed","output":` + output + `,"usage":{"input_tokens":10,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}`
					payload := response
					if stream {
						payload = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
					}
					switch protocol {
					case APIProtocolChatCompletions:
						path, target = "/v1/chat/completions", "/api/v3/chat/completions"
						request = map[string]any{"model": "ep-ark", "stream": stream, "messages": []any{map[string]any{"role": "developer", "content": "preserve my role"}, map[string]any{"role": "user", "content": "hello"}}, "reasoning_effort": effort}
						response = `{"id":"chat1","object":"chat.completion","model":"ep-ark","choices":[{"index":0,"message":` + chat + `,"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`
						payload = response
						if stream {
							payload = "data: " + strings.Replace(strings.Replace(response, `"message":`, `"delta":`, 1), `"chat.completion"`, `"chat.completion.chunk"`, 1) + "\n\ndata: [DONE]\n\n"
						}
					case APIProtocolAnthropic:
						path, target = "/v1/messages", "/api/compatible/v1/messages"
						request = map[string]any{"model": "ep-ark", "stream": stream, "max_tokens": 17, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "thinking": map[string]any{"type": "enabled", "budget_tokens": 10}, "output_config": map[string]any{"effort": effort}}
						response = `{"id":"msg1","type":"message","role":"assistant","model":"ep-ark","content":` + blocks + `,"stop_reason":"tool_use","usage":{"input_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":0,"output_tokens":7}}`
						payload = response
						if stream {
							payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":3,\"output_tokens\":0}}}\n\n"
							for i, b := range gjson.Parse(blocks).Array() {
								payload += fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, b.Raw, i)
							}
							payload += "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
					}
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
					forward := func(body []byte) (*OpenAIForwardResult, string, error) {
						c, w := volcengineUserContext(1, path, body)
						var result *OpenAIForwardResult
						var err error
						switch protocol {
						case APIProtocolResponses:
							result, err = svc.Forward(context.Background(), c, account, body)
						case APIProtocolChatCompletions:
							result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
						case APIProtocolAnthropic:
							result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
						}
						return result, w.Body.String(), err
					}
					body, err := json.Marshal(request)
					require.NoError(t, err)
					result, wire, err := forward(body)
					require.NoError(t, err, wire)
					require.Equal(t, target, upstream.lastReq.URL.Path)
					require.Equal(t, 10, result.Usage.InputTokens)
					require.Equal(t, 3, result.Usage.CacheReadInputTokens)
					require.Equal(t, 7, result.Usage.OutputTokens)
					require.Contains(t, wire, "call_1")
					require.Contains(t, wire, "call_2")
					require.Equal(t, effort != "none", strings.Contains(wire, "original plan"))
					if protocol == APIProtocolChatCompletions {
						require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "messages.0.role").String())
						require.Equal(t, effort, gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
						require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning").Exists())
					}
					if protocol == APIProtocolAnthropic {
						require.NotNil(t, result.ReasoningEffort)
						require.Equal(t, effort, *result.ReasoningEffort)
						require.Equal(t, "fixture-key", upstream.lastReq.Header.Get("x-api-key"))
						if effort == "none" {
							require.Equal(t, "disabled", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
						} else {
							require.Equal(t, "adaptive", gjson.GetBytes(upstream.lastBody, "thinking.type").String())
							require.Equal(t, effort, gjson.GetBytes(upstream.lastBody, "output_config.effort").String())
							require.Contains(t, wire, "native-signature")
						}
						require.Equal(t, int64(17), gjson.GetBytes(upstream.lastBody, "max_tokens").Int())
					}
					request["stream"] = false
					switch protocol {
					case APIProtocolResponses:
						request["input"] = json.RawMessage(`[{"type":"message","role":"user","content":"hello"},` + strings.Trim(output, "[]") + `,{"type":"function_call_output","call_id":"call_2","output":"two"},{"type":"function_call_output","call_id":"call_1","output":"one"}]`)
					case APIProtocolChatCompletions:
						request["messages"] = json.RawMessage(`[{"role":"user","content":"hello"},` + chat + `,{"role":"tool","tool_call_id":"call_2","content":"two"},{"role":"tool","tool_call_id":"call_1","content":"one"}]`)
					case APIProtocolAnthropic:
						request["messages"] = json.RawMessage(`[{"role":"user","content":"hello"},{"role":"assistant","content":` + blocks + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2","content":"two"},{"type":"tool_result","tool_use_id":"call_1","content":"one"}]}]`)
					}
					body, err = json.Marshal(request)
					require.NoError(t, err)
					upstream.resp = &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"capture continuation"}}`))}
					_, _, err = forward(body)
					require.Error(t, err)
					require.Len(t, upstream.requests, 2)
					history := "messages"
					if protocol == APIProtocolResponses {
						history = "input"
					}
					require.JSONEq(t, gjson.GetBytes(body, history).Raw, gjson.GetBytes(upstream.lastBody, history).Raw)
					if effort == "none" {
						require.NotContains(t, string(upstream.lastBody), "reasoning_content")
						require.NotContains(t, string(upstream.lastBody), "reasoning_text")
					}
				})
			}
		}
	}
}

func TestVolcengineNativeEndpointIsolation(t *testing.T) {
	account := volcengineTestAccount()
	account.Credentials["base_url"] = "https://relay.example/v1"
	require.Equal(t, "https://relay.example/v1", account.GetCNProtocolBaseURL(APIProtocolResponses))
	require.Equal(t, "https://relay.example/v1", account.GetOpenAIBaseURL())
	require.Empty(t, account.GetAnthropicProtocolBaseURL())
	account.Credentials["api_base_urls"] = map[string]any{APIProtocolAnthropic: "https://relay.example", APIProtocolResponses: "https://relay.example/v1/responses"}
	require.Equal(t, "https://relay.example/v1", account.GetOpenAIBaseURL())
	require.Equal(t, "https://relay.example", account.GetAnthropicProtocolBaseURL())
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic} {
		account.Credentials["api_base_urls"] = map[string]any{protocol: "https://api.deepseek.com/anthropic"}
		require.Empty(t, account.GetCNProtocolBaseURL(protocol), "never send an Ark credential to the platform default")
	}
	for _, base := range []string{"https://relay.example", "https://relay.example/v1", "https://relay.example/v1/messages"} {
		require.Equal(t, "https://relay.example/v1/messages", nativeAnthropicMessagesURL(account, base))
	}
}

func TestVolcengineNativeMessagesStreamFailure(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0,\"cache_read_input_tokens\":3}}}\n\n"
	for _, tc := range []struct{ name, tail, wantError string }{
		{"eof", "", "missing terminal event"},
		{"partial_stop", "event: message_stop\ndata: {\"type\":\"message_stop\"}", "missing terminal event"},
		{"upstream_error", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n", "returned an error"},
		{"complete", "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(start + tc.tail))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			body := []byte(`{"model":"ep-ark","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`)
			c, w := volcengineUserContext(1, "/v1/messages", body)
			result, err := svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
			require.NotNil(t, result)
			require.Equal(t, 10, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.CacheReadInputTokens)
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, "none", *result.ReasoningEffort)
			if tc.wantError == "" {
				require.NoError(t, err)
				require.Equal(t, 4, result.Usage.OutputTokens)
				require.NotContains(t, w.Body.String(), "event: error")
			} else {
				require.ErrorContains(t, err, tc.wantError)
				require.Equal(t, 1, strings.Count(w.Body.String(), "event: error"))
				require.NotContains(t, w.Body.String(), "message_stop")
				if tc.name == "upstream_error" {
					require.Contains(t, w.Body.String(), "overloaded_error")
					require.Contains(t, w.Body.String(), "busy")
				}
			}
		})
	}
}
