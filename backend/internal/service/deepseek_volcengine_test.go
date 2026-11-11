package service

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
