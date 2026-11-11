package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the whole gateway: a normalizer must not turn accepted plaintext
// into an invalid history before the transport's second validation (#714079).
func TestVolcengineGatewayPlaintextHistoryReplay(t *testing.T) {
	for _, history := range []struct{ name, item, text string }{
		{"raw", `{"type":"reasoning","content":[{"type":"reasoning_text","text":"complete thought"}]}`, "complete thought"},
		{"raw_ciphertext", `{"type":"reasoning","content":[{"type":"reasoning_text","text":"complete thought"}],"encrypted_content":"foreign"}`, "complete thought"},
		{"raw_summary_ciphertext", `{"type":"reasoning","id":"rs_old","content":[{"type":"reasoning_text","text":"complete thought"}],"summary":[{"type":"summary_text","text":"short"}],"encrypted_content":"foreign"}`, "complete thought"},
		{"raw_string_ciphertext", `{"type":"reasoning","content":"complete thought","encrypted_content":"foreign"}`, "complete thought"},
		{"raw_parts_ciphertext", `{"type":"reasoning","content":[{"type":"reasoning_text","text":"first\n"},{"type":"reasoning_text","text":"second"}],"encrypted_content":"foreign"}`, "first\nsecond"},
		{"ark_summary", `{"type":"reasoning","summary":[{"type":"summary_text","text":"complete thought"}]}`, "complete thought"},
		{"ark_summary_null_ciphertext", `{"type":"reasoning","content":null,"summary":[{"type":"summary_text","text":"complete thought"}],"encrypted_content":null}`, "complete thought"},
	} {
		for _, mode := range []string{"ordinary", "tools", "search_history", "server_search", "implicit_server_search", "compaction", "compaction_after_search", "compaction_after_server_search", "compaction_after_implicit_server_search"} {
			for _, stream := range []bool{false, true} {
				for _, effort := range []string{"none", "max"} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", history.name, mode, stream, effort), func(t *testing.T) {
						input := []any{map[string]any{"role": "user", "content": "Synthetic question"}, json.RawMessage(history.item)}
						request := map[string]any{"model": "ep-ark", "stream": stream, "store": false, "reasoning": map[string]any{"effort": effort}}
						if mode == "tools" {
							input = append(input, json.RawMessage(`{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"custom","name":"exec","format":{"type":"text"}},{"type":"tool_search"}]}`),
								json.RawMessage(`{"type":"function_call","call_id":"f1","name":"lookup","arguments":"{}"}`),
								json.RawMessage(`{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"pwd"}`),
								json.RawMessage(`{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"lookup"}}`),
								json.RawMessage(`{"type":"tool_search_output","call_id":"s1","tools":[]}`),
								json.RawMessage(`{"type":"custom_tool_call_output","call_id":"c1","output":"synthetic result"}`),
								json.RawMessage(`{"type":"function_call_output","call_id":"f1","output":"synthetic result"}`))
						}
						if mode == "search_history" || mode == "compaction_after_search" {
							request["tools"] = []any{}
							input = append(input,
								json.RawMessage(`{"type":"tool_search_call","id":"tsc_old","call_id":"s1","execution":"client","arguments":{"query":"lookup"}}`),
								json.RawMessage(`{"type":"tool_search_output","id":"tso_old","call_id":"s1","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
						}
						if strings.Contains(mode, "server_search") {
							request["tools"] = []any{}
							for _, item := range []string{
								`{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["lookup"]}}`,
								`{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
							} {
								if strings.Contains(mode, "implicit") {
									item = strings.ReplaceAll(item, `"execution":"server",`, "")
								}
								input = append(input, json.RawMessage(item))
							}
						}
						prompt := "Reply with OK."
						compaction := strings.HasPrefix(mode, "compaction")
						if compaction {
							prompt = volcengineCompactionPrompt
						}
						input = append(input, map[string]any{"role": "assistant", "content": "Synthetic previous answer"}, map[string]any{"role": "user", "content": prompt})
						request["input"] = input
						body, err := json.Marshal(request)
						require.NoError(t, err)
						upstream := &httpUpstreamRecorder{resp: volcengineCompactionTestResponse(t, "Synthetic complete answer", "completed", stream)}
						svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
						c, recorder := volcengineUserContext(1, "/v1/responses", body)
						_, err = svc.Forward(context.Background(), c, volcengineTestAccount(), body)
						require.NoError(t, err, recorder.Body.String())
						require.Len(t, upstream.requests, 1)
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Contains(t, recorder.Body.String(), "Synthetic complete answer")
						reasoning := gjson.GetBytes(upstream.lastBody, "input.#(type==reasoning)")
						var text strings.Builder
						for _, part := range reasoning.Get("summary").Array() {
							text.WriteString(part.Get("text").String())
						}
						require.Equal(t, history.text, text.String())
						require.Empty(t, reasoning.Get("encrypted_content").String())
						require.NoError(t, validateVolcengineResponsesRequest(volcengineTestAccount(), upstream.lastBody))
						if mode == "search_history" || mode == "compaction_after_search" {
							require.Empty(t, gjson.GetBytes(upstream.lastBody, "tools").Array())
							require.False(t, gjson.GetBytes(upstream.lastBody, "input.#(type==tool_search_call)").Exists())
							require.Equal(t, "s1", gjson.GetBytes(upstream.lastBody, "input.#(type==function_call_output).call_id").String())
						}
						if strings.Contains(mode, "server_search") {
							for _, typ := range []string{"tool_search_call", "tool_search_output"} {
								path := "input.#(type==" + typ + ")"
								require.JSONEq(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(upstream.lastBody, path).Raw)
							}
						}
						if compaction {
							require.NotContains(t, recorder.Body.String(), "THOUGHT_MUST_NOT_BECOME_SUMMARY")
						}
					})
				}
			}
		}
	}
}

func TestVolcenginePlaintextCiphertextCleanupScope(t *testing.T) {
	body := []byte(`{"store":false,"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"complete thought"}],"summary":[{"type":"summary_text","text":"short"}],"encrypted_content":"foreign"}]}`)
	for _, platform := range []string{PlatformDeepseek, PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformGrok} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", platform, enabled), func(t *testing.T) {
				account := volcengineTestAccount()
				account.Platform = platform
				account.Credentials[DeepSeekVolcengineCompatibilityCredential] = enabled
				got := normalizeDeepSeekResponsesRequestBody(account, body)
				if platform == PlatformDeepseek && enabled {
					require.False(t, gjson.GetBytes(got, "input.0.encrypted_content").Exists())
					require.Equal(t, "complete thought", gjson.GetBytes(got, "input.0.summary.0.text").String())
				} else {
					require.JSONEq(t, string(body), string(got))
				}
			})
		}
	}
	account := volcengineTestAccount()
	account.Credentials[DeepSeekVolcengineCompatibilityCredential] = false
	account.Credentials["base_url"] = "https://api.deepseek.com"
	require.JSONEq(t, string(body), string(normalizeDeepSeekResponsesRequestBody(account, body)))
}

func TestVolcengineInvalidLoweredToolHistoryReturnsBadRequest(t *testing.T) {
	for _, history := range []string{
		`[{"type":"tool_search_call","call_id":"s1","arguments":{}}]`,
		`[{"type":"tool_search_output","call_id":"s1","tools":[]}]`,
		`[{"type":"tool_search_call","execution":"client","arguments":{}}]`,
	} {
		body := []byte(`{"model":"ep-ark","tools":[{"type":"tool_search"}],"input":` + history + `}`)
		upstream := &httpUpstreamRecorder{}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		c, recorder := volcengineUserContext(1, "/v1/responses", body)
		_, err := svc.Forward(context.Background(), c, volcengineTestAccount(), body)
		require.Error(t, err)
		require.Empty(t, upstream.requests)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		_, upstreamFailure := c.Get(OpsUpstreamErrorsKey)
		require.False(t, upstreamFailure, "local validation must not be reported as a provider failure")
	}
}
