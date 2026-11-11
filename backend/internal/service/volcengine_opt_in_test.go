package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Session affinity is unrelated to thinking. Any accidental reasoning cache
// access panics so the full-history continuation tests prove cache independence.
type volcengineNoReasoningCache struct{ GatewayCache }

func (*volcengineNoReasoningCache) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}
func (*volcengineNoReasoningCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, ErrStickySessionNotFound
}
func (*volcengineNoReasoningCache) SetReasoningContent(context.Context, string, string, time.Duration) error {
	panic("unexpected reasoning cache write")
}
func (*volcengineNoReasoningCache) GetReasoningContent(context.Context, string) (string, error) {
	panic("unexpected reasoning cache read")
}

func volcengineUserContext(user int64, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	SetOpenAIHTTPResponseOwner(c, user, user+100)
	return c, recorder
}

func TestVolcengineNewBehaviorStrictAccountScope(t *testing.T) {
	for _, platform := range []string{PlatformDeepseek, PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformGrok} {
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			for _, flag := range []any{nil, false, true, "true"} {
				t.Run(fmt.Sprintf("%s/%s/%v", platform, kind, flag), func(t *testing.T) {
					account := volcengineTestAccount()
					account.Platform, account.Type = platform, kind
					account.Credentials[DeepSeekVolcengineCompatibilityCredential] = flag
					account.Credentials["api_protocol"] = APIProtocolChatCompletions
					enabled := platform == PlatformDeepseek && kind == AccountTypeAPIKey && flag == true
					require.Equal(t, enabled, account.IsDeepSeekVolcengineCompatibilityEnabled())
					body := []byte(`{"input":[{"type":"reasoning","id":"missing","summary":[]}],"thinking":{"type":"enabled"},"output_config":{"effort":"max"}}`)
					err := validateVolcengineResponsesRequest(account, body)
					if enabled {
						require.Error(t, err)
						require.Equal(t, APIProtocolAdaptive, account.GetAPIProtocol())
					} else {
						require.NoError(t, err)
					}
					normalized, err := normalizeVolcengineRequestBody(account, body, APIProtocolAnthropic)
					require.NoError(t, err)
					if enabled {
						require.Equal(t, "adaptive", gjson.GetBytes(normalized, "thinking.type").String())
					} else {
						require.Equal(t, body, normalized)
					}
					transport := &httpUpstreamRecorder{}
					if !enabled {
						require.Same(t, transport, accountVolcengineHTTP(transport, account))
					}
				})
			}
		}
	}
}

func TestVolcengineRequestHistoryPolicy(t *testing.T) {
	for _, tc := range []struct{ name, body, errorText string }{
		{"text", `{"input":"hello"}`, ""},
		{"ordinary_assistant", `{"input":[{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`, ""},
		{"full_summary", `{"input":[{"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"  plan\n"}]}]}`, ""},
		{"raw_with_foreign_summary", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"full"}],"summary":[{"type":"summary_text","text":"short"}],"encrypted_content":"foreign"}]}`, ""},
		{"tool_pairs", `{"input":[{"type":"function_call","call_id":"1"},{"type":"function_call","call_id":"2"},{"type":"function_call_output","call_id":"2","output":"b"},{"type":"function_call_output","call_id":"1","output":"a"}]}`, ""},
		{"id_only", `{"input":[{"type":"reasoning","id":"lost","summary":[]}]}`, "complete reasoning history"},
		{"encrypted", `{"input":[{"type":"reasoning","encrypted_content":"foreign"}]}`, "recoverable plaintext"},
		{"real_summary", `{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"short"}],"encrypted_content":"foreign"}]}`, "recoverable plaintext"},
		{"store", `{"input":"hi","store":true}`, "stateless full history"},
		{"previous", `{"input":"hi","previous_response_id":"r"}`, "stateless full history"},
		{"conversation", `{"input":"hi","conversation":{"id":"c"}}`, "stateless full history"},
		{"reference", `{"input":[{"type":"item_reference","id":"r"}]}`, "complete plaintext history"},
		{"compact", `{"input":[{"type":"compaction_trigger"}]}`, "complete plaintext history"},
		{"hidden_reasoning", `{"input":"hi","reasoning":{"effort":"high","summary":"none"}}`, "hide thinking locally"},
		{"disabled_reasoning", `{"input":"hi","reasoning":{"effort":"none","summary":"none"}}`, ""},
		{"tool_result_without_call", `{"input":[{"type":"function_call_output","call_id":"1"}]}`, "complete tool history"},
		{"tool_without_result", `{"input":[{"type":"function_call","call_id":"1"}]}`, "missing results"},
		{"duplicate_call", `{"input":[{"type":"function_call","call_id":"1"},{"type":"function_call","call_id":"1"}]}`, "duplicate call_id"},
		{"chat_shape", `{"messages":[{"role":"user","content":"hi"}]}`, "Responses requires input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVolcengineResponsesRequest(volcengineTestAccount(), []byte(tc.body))
			if tc.errorText == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.errorText)
			}
			disabled := volcengineTestAccount()
			disabled.Credentials[DeepSeekVolcengineCompatibilityCredential] = false
			require.NoError(t, validateVolcengineResponsesRequest(disabled, []byte(tc.body)))
		})
	}
}

func TestVolcengineNativeSSEToJSONPreservesReasoningWithoutCache(t *testing.T) {
	svc := &OpenAIGatewayService{cache: &volcengineNoReasoningCache{}}
	payload, _ := volcengineSuccessFixture(true)
	c, recorder := volcengineUserContext(1, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}}
	_, err := svc.handleSSEToJSON(resp, c, volcengineTestAccount(), []byte(payload), "ep-ark", "ep-ark")
	require.NoError(t, err)
	require.Equal(t, "original plan", gjson.GetBytes(recorder.Body.Bytes(), "output.0.summary.0.text").String())
}

func TestVolcengineSSEFramingAndPartialEvent(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		input := "\ufeff" + strings.Join([]string{": heartbeat", "", "event: response.reasoning_text.delta", "data: {\"type\":\"response.reasoning_text.delta\",", "data: \"delta\":\"中文\",\"output_index\":0}", "", "event: response.completed", "data: {\"type\":\"response.completed\"}", "", ""}, newline)
		body := newVolcengineSSEBody(io.NopCloser(&oneByteReader{data: []byte(input)}))
		got, err := io.ReadAll(body)
		require.NoError(t, err)
		require.Contains(t, string(got), ": heartbeat\n\n")
		require.Contains(t, string(got), `data: {"type":"response.reasoning_text.delta","delta":"中文","output_index":0}`)
		require.Contains(t, string(got), "event: response.completed\n")
	}
	partial := newVolcengineSSEBody(io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\"}")))
	got, err := io.ReadAll(partial)
	require.NoError(t, err)
	require.Empty(t, got, "EOF cannot complete an unterminated event")
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func volcengineSuccessFixture(stream bool) (string, string) {
	response := `{"id":"r","object":"response","model":"ep-ark","status":"completed","created_at":1,"output":[{"type":"reasoning","id":"rs","status":"completed","summary":[{"type":"summary_text","text":"original plan"}]},{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer"}]}],"usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}`
	if !stream {
		return response, "application/json"
	}
	events := []string{
		`{"type":"response.created","response":{"id":"r","model":"ep-ark","created_at":1,"status":"in_progress","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"original plan"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","status":"completed","summary":[{"type":"summary_text","text":"original plan"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"final answer"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"final answer"}]}}`,
		`{"type":"response.completed","response":` + response + `}`,
	}
	var result strings.Builder
	for _, event := range events {
		fmt.Fprintf(&result, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
	}
	return result.String(), "text/event-stream"
}

func TestVolcengineGatewayRejectsUnsupportedRequestsBeforeUpstream(t *testing.T) {
	for _, tc := range []struct{ path, body, errorText string }{
		{"/v1/responses", `{"model":"ep-ark","input":[{"type":"reasoning","id":"lost"}]}`, "complete reasoning history"},
		{"/v1/responses", `{"model":"ep-ark","input":[{"type":"reasoning","encrypted_content":"foreign"}]}`, "complete reasoning history"},
		{"/v1/responses", `{"model":"ep-ark","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"abbreviated"}],"encrypted_content":"foreign"}]}`, "complete reasoning history"},
		{"/v1/responses", `{"model":"ep-ark","input":"hi","previous_response_id":"old"}`, "stateless full history"},
		{"/v1/responses/compact", `{"model":"ep-ark","input":"hi"}`, "does not support /responses/compact"},
		{"/v1/responses", `{"model":"ep-ark","input":[{"type":"additional_tools","tools":"bad"}]}`, "additional_tools.tools must be an array"},
	} {
		t.Run(tc.path+tc.errorText, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
			c, recorder := volcengineUserContext(1, tc.path, []byte(tc.body))
			_, err := svc.Forward(context.Background(), c, volcengineTestAccount(), []byte(tc.body))
			require.ErrorContains(t, err, tc.errorText)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Empty(t, upstream.requests)
		})
	}
}

func TestVolcengineOptInDoesNotAffectOtherChatAccounts(t *testing.T) {
	for _, platform := range []string{PlatformDeepseek, PlatformOpenAI, PlatformKimi} {
		account := volcengineTestAccount()
		account.Platform = platform
		account.Credentials["api_protocol"] = APIProtocolChatCompletions
		account.Credentials[DeepSeekVolcengineCompatibilityCredential] = platform != PlatformDeepseek
		body := []byte(`{"model":"deepseek-v4.1","messages":[{"role":"user","content":"hi"}]}`)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"capture"}}`))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		c, _ := volcengineUserContext(1, "/v1/chat/completions", body)
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
		require.Error(t, err)
		require.Len(t, upstream.requests, 1)
		require.NotContains(t, err.Error(), "Responses requires input")
	}
}

func TestVolcengineAdditionalToolsUseNativeResponses(t *testing.T) {
	for _, tool := range []string{
		`{"type":"function","name":"lookup","parameters":{"type":"object"},"provider_extension":9007199254740993}`,
		`{"type":"custom","name":"exec","format":{"type":"text"}}`,
		`{"type":"namespace","name":"shell","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
		`{"type":"tool_search"}`,
	} {
		body := []byte(`{"model":"gpt-5.4","input":[{"type":"additional_tools","tools":[` + tool + `]},{"role":"user","content":"hi"}]}`)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"capture"}}`))}}
		account := volcengineTestAccount()
		account.Credentials["api_protocol"] = APIProtocolChatCompletions
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		c, _ := volcengineUserContext(1, "/v1/responses", body)
		_, err := svc.Forward(context.Background(), c, account, body)
		require.Error(t, err)
		require.Len(t, upstream.requests, 1)
		require.Equal(t, "/api/v3/responses", upstream.lastReq.URL.Path)
		require.NotContains(t, string(upstream.lastBody), "additional_tools")
		if gjson.Get(tool, "type").String() == "tool_search" {
			require.Equal(t, "function", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
			require.Equal(t, "tool_search", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
		} else {
			require.JSONEq(t, tool, gjson.GetBytes(upstream.lastBody, "tools.0").Raw)
		}
		if gjson.Get(tool, "name").String() == "lookup" {
			require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.lastBody, "tools.0.provider_extension").Raw)
		}
	}
}

func TestVolcengineTransportRejectsRemoteCompaction(t *testing.T) {
	for _, path := range []string{"/v1/responses/compact"} {
		upstream := &httpUpstreamRecorder{}
		req := httptest.NewRequest(http.MethodPost, "https://ark.example"+path, strings.NewReader(`{"input":"hello"}`))
		_, err := accountVolcengineHTTP(upstream, volcengineTestAccount()).Do(req, "", 144, 1)
		require.Error(t, err)
		require.Empty(t, upstream.requests)
	}
}

func TestVolcengineGatewaySSEFailuresNeverComplete(t *testing.T) {
	prefix := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
	for _, terminal := range []string{
		"event: response.cancelled\ndata: {\"type\":\"response.cancelled\",\"response\":{\"id\":\"r\",\"status\":\"cancelled\"}}\n\n",
		"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"code\":\"invalid_request_error\",\"message\":\"fixture failure\"}}}\n\n",
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"code\":\"invalid_request_error\",\"message\":\"fixture failure\"}}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"code\":\"invalid_request_error\",\"message\":\"fixture failure\"}\n\n",
		// Valid terminal JSON with no blank line is still an incomplete SSE frame.
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}",
	} {
		for _, path := range []string{"/v1/responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, strings.Split(terminal, "\n")[0]), func(t *testing.T) {
					payload := "\ufeff" + strings.ReplaceAll(prefix+terminal, "\n", "\r\n")
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
					body := []byte(fmt.Sprintf(`{"model":"ep-ark","stream":%t,"input":"hi"}`, stream))
					c, recorder := volcengineUserContext(1, path, body)
					_, err := svc.Forward(context.Background(), c, volcengineTestAccount(), body)
					require.Error(t, err, recorder.Body.String())
					require.Len(t, upstream.requests, 1)
					require.NotContains(t, recorder.Body.String(), "response.completed")
					require.NotContains(t, recorder.Body.String(), "message_stop")
				})
			}
		}
	}
}

func TestVolcengineMessagesFullHistoryWithGPTAlias(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": "oldest prompt"}}
	for i := 0; i < 16; i++ {
		messages = append(messages, map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": fmt.Sprintf("plan %d", i)},
			map[string]any{"type": "text", "text": fmt.Sprintf("answer %d", i)},
		}}, map[string]any{"role": "user", "content": fmt.Sprintf("continue %d", i)})
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-5.4", "max_tokens": 17, "messages": messages, "output_config": map[string]any{"effort": "max"}})
	require.NoError(t, err)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"capture"}}`))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
	c, _ := volcengineUserContext(1, "/v1/messages", body)
	_, err = svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "fixture-session", "")
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/api/compatible/v1/messages", upstream.lastReq.URL.Path)
	require.Contains(t, string(upstream.lastBody), "oldest prompt")
	require.Contains(t, string(upstream.lastBody), "plan 0")
	require.Contains(t, string(upstream.lastBody), "plan 15")
	require.Equal(t, "max", gjson.GetBytes(upstream.lastBody, "output_config.effort").String())
	require.Equal(t, int64(17), gjson.GetBytes(upstream.lastBody, "max_tokens").Int())
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
}

func TestVolcengineNativeSSEToJSONPreservesIncomplete(t *testing.T) {
	response := `{"id":"r","object":"response","status":"incomplete","output":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"partial thought"}]}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":17}}`
	body := []byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":" + response + "}\n\n")
	c, recorder := volcengineUserContext(1, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}}
	svc := &OpenAIGatewayService{cache: &volcengineNoReasoningCache{}}
	result, err := svc.handleSSEToJSON(resp, c, volcengineTestAccount(), body, "ep-ark", "ep-ark")
	require.NoError(t, err)
	require.Equal(t, 17, result.usage.OutputTokens)
	require.Equal(t, "incomplete", gjson.GetBytes(recorder.Body.Bytes(), "status").String())
	require.Equal(t, "max_output_tokens", gjson.GetBytes(recorder.Body.Bytes(), "incomplete_details.reason").String())
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
}
