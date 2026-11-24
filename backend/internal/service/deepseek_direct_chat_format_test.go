//go:build unit

package service

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDirectChatDeepSeekResponseFormat(t *testing.T) {
	const flash = "deepseek-flash"
	const official = "https://api.deepseek.com"
	const relay = "https://relay.example.com"
	const strictSchema = `{"type":"json_schema","json_schema":{"name":"reply","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}}`
	const jsonReply = `{"id":"reply","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`
	for _, tc := range []struct {
		name, chatBase, requested, mapped, format string
		reject                                    bool
	}{
		{"omitted", official, flash, flash, "", false},
		{"plain_text", official, flash, flash, `{"type":"text"}`, false},
		{"json_object", official, flash, flash, `{"type":"json_object"}`, false},
		{"strict_schema", official, flash, flash, strictSchema, true},
		{"non_strict_schema", official, flash, flash, strings.Replace(strictSchema, `"strict":true`, `"strict":false`, 1), true},
		{"mapped_flash_schema", official, "chat-alias", flash, strictSchema, true},
		{"relay_override", relay, flash, flash, strictSchema, false},
		{"other_model_at_same_host", official, "custom-model", "custom-model", strictSchema, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := rawChatCompletionsTestAccount()
			account.Platform = PlatformDeepseek
			account.Credentials["api_protocol"] = APIProtocolAdaptive
			account.Credentials["base_url"] = official
			account.Credentials["api_base_urls"] = map[string]any{APIProtocolChatCompletions: tc.chatBase}
			account.Credentials["model_mapping"] = map[string]any{tc.requested: tc.mapped}
			request := map[string]any{"model": tc.requested, "messages": []map[string]string{{"role": "user", "content": "Reply with a JSON object"}}}
			if tc.format != "" {
				request["response_format"] = json.RawMessage(tc.format)
			}
			body, err := json.Marshal(request)
			require.NoError(t, err)
			c, rec := volcengineUserContext(1, "/v1/chat/completions", body)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(jsonReply))}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			result, err := svc.ForwardAsChatCompletions(t.Context(), c, account, body, "", "")
			if tc.reject {
				require.Error(t, err)
				require.Nil(t, result)
				require.Nil(t, upstream.lastReq, "unsupported constraints must never be sent or silently discarded")
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
				msg := gjson.Get(rec.Body.String(), "error.message").String()
				require.Contains(t, msg, tc.requested)
				require.Contains(t, msg, "response_format.type")
				require.Contains(t, msg, "json_schema")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, tc.chatBase, "https://"+upstream.lastReq.URL.Host)
			require.Equal(t, tc.mapped, gjson.GetBytes(upstream.lastBody, "model").String())
			got := gjson.GetBytes(upstream.lastBody, "response_format")
			if tc.format == "" {
				require.False(t, got.Exists())
			} else {
				require.JSONEq(t, tc.format, got.Raw)
			}
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "{}", gjson.Get(rec.Body.String(), "choices.0.message.content").String())
		})
	}
}
