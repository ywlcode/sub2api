package service

import (
	"bytes"
	"compress/gzip"
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

func volcengineTestAccount() *Account {
	return &Account{ID: 144, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "fixture-key", "api_protocol": APIProtocolResponses,
			"base_url": "https://ark.cn-beijing.volces.com/api/v3", DeepSeekVolcengineCompatibilityCredential: true}}
}

func TestVolcengineCompatibilityScope(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"off"},"input":[{"role":"assistant","content":"hello","internal_chat_message_metadata_passthrough":{}}]}`)
	for _, account := range []*Account{nil, {}, {Platform: PlatformDeepseek, Type: AccountTypeAPIKey},
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{DeepSeekVolcengineCompatibilityCredential: true}},
		{Platform: PlatformDeepseek, Type: AccountTypeOAuth, Credentials: map[string]any{DeepSeekVolcengineCompatibilityCredential: true}},
		{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{DeepSeekVolcengineCompatibilityCredential: "true"}},
		{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{DeepSeekVolcengineCompatibilityCredential: false}}} {
		got, err := normalizeVolcengineRequestBody(account, body, APIProtocolResponses)
		require.NoError(t, err)
		require.Equal(t, body, got)
	}
}

func TestVolcengineResponsesHistory(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"input":[
		{"role":"assistant","phase":"commentary","internal_chat_message_metadata_passthrough":{"x":1},"content":[{"type":"output_text","text":"<thinking>literal example</thinking>","logprobs":[]}]},
		{"role":"user","content":"internal_chat_message_metadata_passthrough"},
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"internal_chat_message_metadata_passthrough\":123}"},
		{"call_id":"call_1","output":"tool result"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"real reasoning"}]},
		{"role":"assistant","call_id":"ambiguous","content":"do not guess"}
	]}`)
	got, err := normalizeVolcengineRequestBody(volcengineTestAccount(), body, APIProtocolResponses)
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", gjson.GetBytes(got, "seed").Raw)
	require.Equal(t, "message", gjson.GetBytes(got, "input.0.type").String())
	require.Equal(t, "message", gjson.GetBytes(got, "input.1.type").String())
	require.Equal(t, "commentary", gjson.GetBytes(got, "input.0.phase").String())
	require.False(t, gjson.GetBytes(got, "input.0.internal_chat_message_metadata_passthrough").Exists())
	require.False(t, gjson.GetBytes(got, "input.0.content.0.logprobs").Exists())
	require.Equal(t, "<thinking>literal example</thinking>", gjson.GetBytes(got, "input.0.content.0.text").String())
	require.Equal(t, "internal_chat_message_metadata_passthrough", gjson.GetBytes(got, "input.1.content").String())
	require.Equal(t, gjson.GetBytes(body, "input.2.arguments").String(), gjson.GetBytes(got, "input.2.arguments").String())
	require.False(t, gjson.GetBytes(got, "input.3.type").Exists())
	require.Equal(t, "reasoning", gjson.GetBytes(got, "input.4.type").String())
	require.False(t, gjson.GetBytes(got, "input.5.type").Exists())
	again, err := normalizeVolcengineRequestBody(volcengineTestAccount(), got, APIProtocolResponses)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestVolcengineReasoningAliasesAndStrength(t *testing.T) {
	account := volcengineTestAccount()
	for _, effort := range []string{"off", "disabled", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			body := []byte(`{"reasoning":{"effort":"` + effort + `","summary":"auto"},"reasoning_effort":"low","thinking":{"type":"enabled"}}`)
			got, err := normalizeVolcengineRequestBody(account, body, APIProtocolResponses)
			require.NoError(t, err)
			want := effort
			if effort == "off" || effort == "disabled" {
				want = "none"
			}
			require.Equal(t, want, gjson.GetBytes(got, "reasoning.effort").String())
			require.False(t, gjson.GetBytes(got, "reasoning_effort").Exists())
			require.False(t, gjson.GetBytes(got, "thinking").Exists())
			filtered, err := filterOpenAIResponsesNoneReasoningEffortForAccount(account, got)
			require.NoError(t, err)
			require.Equal(t, got, filtered)
			var object map[string]any
			require.NoError(t, json.Unmarshal(got, &object))
			deleteOpenAIResponsesNoneReasoningEffortFromObject(account, object)
			encoded, err := json.Marshal(object)
			require.NoError(t, err)
			require.Equal(t, want, gjson.GetBytes(encoded, "reasoning.effort").String())
		})
	}
	got, err := normalizeVolcengineRequestBody(account, []byte(`{"thinking":{"type":"disabled","budget_tokens":99},"reasoning":{"effort":"max"}}`), APIProtocolResponses)
	require.NoError(t, err)
	require.Equal(t, "none", gjson.GetBytes(got, "reasoning.effort").String())
	for _, body := range []string{`{"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`, `{"output_config":{"effort":"off"}}`} {
		got, err := normalizeVolcengineRequestBody(account, []byte(body), APIProtocolAnthropic)
		require.NoError(t, err)
		require.Equal(t, "disabled", gjson.GetBytes(got, "thinking.type").String())
		require.False(t, gjson.GetBytes(got, "output_config.effort").Exists())
	}
}

func TestVolcengineGatewayIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path, body, effort string
		reasoning                bool
	}{
		{"responses", "/v1/responses", `{"model":"deepseek-v4-1-flash-261017","reasoning":{"effort":"disabled"},"input":[{"role":"assistant","content":[{"type":"output_text","text":"answer","logprobs":[]}],"internal_chat_message_metadata_passthrough":{}}]}`, "none", false},
		{"legacy_messages", "/v1/responses", `{"model":"deepseek-v4-1-flash-261017","reasoning_effort":"max","messages":[{"role":"assistant","reasoning_content":"plan","content":"answer"}]}`, "max", true},
		{"chat", "/v1/chat/completions", `{"model":"deepseek-v4-1-flash-261017","reasoning_effort":"high","messages":[{"role":"assistant","reasoning_content":"plan","content":[{"type":"thinking","thinking":"details"},{"type":"text","text":"answer"}]}]}`, "high", true},
		{"responses_shaped_chat", "/v1/chat/completions", `{"model":"deepseek-v4-1-flash-261017","reasoning":{"effort":"max","summary":"auto"},"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},{"role":"assistant","content":"answer","internal_chat_message_metadata_passthrough":{}}]}`, "max", true},
		{"chat_disabled", "/v1/chat/completions", `{"model":"deepseek-v4-1-flash-261017","thinking":{"type":"disabled"},"reasoning_effort":"max","messages":[{"role":"user","content":"hello"}]}`, "none", false},
		{"messages", "/v1/messages", `{"model":"deepseek-v4-1-flash-261017","max_tokens":1000,"output_config":{"effort":"max"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"answer"}]}]}`, "max", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			// Stop after capturing the final request; no paid inference or success-response fixture is needed.
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture stop"}}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := volcengineTestAccount()
			var err error
			switch tc.path {
			case "/v1/responses":
				_, err = svc.Forward(context.Background(), c, account, body)
			case "/v1/chat/completions":
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			case "/v1/messages":
				_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			}
			require.Error(t, err)
			if tc.path == "/v1/chat/completions" || tc.name == "legacy_messages" {
				require.Empty(t, upstream.requests)
				require.Equal(t, http.StatusBadRequest, c.Writer.Status())
				require.Contains(t, err.Error(), "chat completions is not supported")
				return
			}
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, "/api/v3/responses", upstream.lastReq.URL.Path)
			require.Equal(t, tc.effort, gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			require.NotContains(t, string(upstream.lastBody), "internal_chat_message_metadata_passthrough")
			require.NotContains(t, string(upstream.lastBody), "<thinking>")
			if tc.reasoning {
				require.Equal(t, "reasoning", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
				require.Equal(t, "plan", gjson.GetBytes(upstream.lastBody, "input.0.summary.0.text").String())
			} else if tc.name == "responses" {
				require.Equal(t, "message", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
			}
		})
	}
}

func TestVolcengineHistoryKeepsToolsAndLiteralTags(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"plan"},{"type":"reasoning_text","text":"details"}]},{"role":"assistant","content":[{"type":"output_text","text":"<thinking>quoted example</thinking>"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"result"}]}`)
	got, err := normalizeVolcengineRequestBody(volcengineTestAccount(), body, APIProtocolResponses)
	require.NoError(t, err)
	require.Equal(t, "plan", gjson.GetBytes(got, "input.0.summary.0.text").String())
	require.Equal(t, "details", gjson.GetBytes(got, "input.0.summary.1.text").String())
	require.Equal(t, "<thinking>quoted example</thinking>", gjson.GetBytes(got, "input.1.content.0.text").String())
	require.Equal(t, "function_call", gjson.GetBytes(got, "input.2.type").String())
	require.Equal(t, "call_1", gjson.GetBytes(got, "input.3.call_id").String())
	require.NoError(t, validateVolcengineResponsesRequest(volcengineTestAccount(), got))
}

func TestVolcengineAnthropicThinkingHistory(t *testing.T) {
	for _, signature := range []string{"", "opaque-provider-signature", "gAAAA-foreign"} {
		t.Run(signature, func(t *testing.T) {
			content, err := json.Marshal([]apicompat.AnthropicContentBlock{
				{Type: "thinking", Thinking: "plan", Signature: signature},
				{Type: "tool_use", ID: "call_1", Name: "lookup", Input: json.RawMessage(`{}`)},
			})
			require.NoError(t, err)
			resp, err := anthropicToResponsesForAccount(nil, volcengineTestAccount(), &apicompat.AnthropicRequest{
				Model: "deepseek-v4-1-flash-261017", Messages: []apicompat.AnthropicMessage{
					{Role: "assistant", Content: content},
					{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]`)},
				},
			})
			require.NoError(t, err)
			require.Equal(t, "plan", gjson.GetBytes(resp.Input, "0.summary.0.text").String())
			require.Equal(t, "function_call", gjson.GetBytes(resp.Input, "1.type").String())
			require.Equal(t, "call_1", gjson.GetBytes(resp.Input, "1.call_id").String())
			require.False(t, gjson.GetBytes(resp.Input, "0.encrypted_content").Exists())
		})
	}
}

func TestVolcengineGatewayEffortAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, upstreamProtocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolAdaptive} {
		for _, inbound := range []string{"/v1/responses", "/v1/messages"} {
			for _, effort := range []string{"off", "high", "max", "xhigh"} {
				t.Run(upstreamProtocol+inbound+"/"+effort, func(t *testing.T) {
					// An endpoint alias must retain the same effort as a named model.
					payload := map[string]any{"model": "ep-volcengine-fixture"}
					switch inbound {
					case "/v1/responses":
						payload["input"] = []any{map[string]any{"role": "user", "content": "hello"}}
						payload["reasoning"] = map[string]any{"effort": effort}
					case "/v1/chat/completions":
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						payload["reasoning_effort"] = effort
					case "/v1/messages":
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						payload["max_tokens"] = 1000
						payload["output_config"] = map[string]any{"effort": effort}
					}
					body, err := json.Marshal(payload)
					require.NoError(t, err)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, inbound, bytes.NewReader(body))
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")),
					}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
					account := volcengineTestAccount()
					account.Credentials["api_protocol"] = upstreamProtocol
					switch inbound {
					case "/v1/responses":
						_, err = svc.Forward(context.Background(), c, account, body)
					case "/v1/chat/completions":
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					case "/v1/messages":
						_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
					}
					require.Error(t, err) // The fixture deliberately stops after capture.
					require.NotNil(t, upstream.lastReq)
					want := effort
					if effort == "off" {
						want = "none"
					}
					require.Equal(t, "/api/v3/responses", upstream.lastReq.URL.Path)
					require.Equal(t, want, gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
					require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
				})
			}
		}
	}
}

func TestVolcengineTransportBeforeGzipAndProbe(t *testing.T) {
	account := volcengineTestAccount()
	account.Extra = map[string]any{AccountExtraRequestGzipEnabled: true}
	body := []byte(`{"reasoning_effort":"off","input":[{"role":"user","content":"` + strings.Repeat("hello", 1024) + `","internal_chat_message_metadata_passthrough":{}}]}`)
	calls := 0
	upstream := gzipTestHTTP{send: func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
		gz, err := gzip.NewReader(req.Body)
		require.NoError(t, err)
		got, err := io.ReadAll(gz)
		require.NoError(t, err)
		require.NoError(t, gz.Close())
		require.NoError(t, req.Body.Close())
		require.Equal(t, "none", gjson.GetBytes(got, "reasoning.effort").String())
		require.False(t, gjson.GetBytes(got, "input.0.internal_chat_message_metadata_passthrough").Exists())
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
	}}
	for _, probe := range []bool{false, true} {
		req := gzipTestRequest(t, body)
		req.URL.Path = "/api/v3/responses"
		var err error
		if probe {
			svc := &AccountTestService{httpUpstream: upstream}
			_, err = svc.accountHTTPUpstream(account).Do(req, "", account.ID, 1)
		} else {
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			_, err = svc.doOpenAIUpstream(req, "", account)
		}
		require.NoError(t, err)
		replay, err := req.GetBody()
		require.NoError(t, err)
		original, err := io.ReadAll(replay)
		require.NoError(t, err)
		require.NoError(t, replay.Close())
		require.Equal(t, body, original)
	}
	require.Equal(t, 2, calls)
}
