package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexPageContextFixture = `{"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"stream":false,"input":[{"type":"function_call_output","name":"codex_apps_open_page","namespace":"agent","output":"{\"page_id\":null}"},{"type":"function_call_output","name":"codex_apps_selected_page_part_1_of_1","namespace":"agent","output":"Companion Page snapshot part 1 of 1:\n{\"page_id\":null,\"document_view\":null}"},{"role":"user","content":"hello"}]}`

func TestNormalizeCodexPageContextPublicReproduction(t *testing.T) {
	body := []byte(codexPageContextFixture)
	original := bytes.Clone(body)
	got, names := normalizeCodexPageContext(body)
	require.Equal(t, []string{"codex_apps_open_page", "codex_apps_selected_page_part_1_of_1"}, names)
	require.Equal(t, original, body)
	for _, index := range []string{"0", "1"} {
		item := gjson.GetBytes(got, "input."+index)
		require.Equal(t, "message", item.Get("type").String())
		require.Equal(t, "user", item.Get("role").String())
		require.Contains(t, item.Get("content.0.text").String(), "agent."+gjson.GetBytes(body, "input."+index+".name").String())
		require.Equal(t, gjson.GetBytes(body, "input."+index+".output").String(), item.Get("content.1.text").String())
		require.False(t, item.Get("call_id").Exists())
		require.False(t, item.Get("output").Exists())
	}
	require.JSONEq(t, gjson.GetBytes(body, "input.2").Raw, gjson.GetBytes(got, "input.2").Raw)
	again, names := normalizeCodexPageContext(got)
	require.Equal(t, got, again)
	require.Empty(t, names)
}

func TestNormalizeCodexPageContextLeavesUnsupportedItemsUntouched(t *testing.T) {
	tests := []struct {
		name, path string
		value      any
	}{
		{"paired", "call_id", "call_real"},
		{"numeric call id", "call_id", 123},
		{"wrong namespace", "namespace", "codex_app"},
		{"unknown tool", "name", "other_tool"},
		{"invalid page index", "name", "codex_apps_selected_page_part_0_of_1"},
		{"index exceeds total", "name", "codex_apps_selected_page_part_2_of_1"},
		{"invalid page suffix", "name", "codex_apps_selected_page_part_1_of_1_extra"},
		{"wrong item type", "type", "custom_tool_call_output"},
		{"null output", "output", nil},
		{"structured output", "output", map[string]any{"page_id": nil}},
		{"empty parts", "output", []any{}},
		{"media parts", "output", []any{map[string]any{"type": "input_image", "image_url": "https://example.test/image"}}},
		{"mixed malformed parts", "output", []any{map[string]any{"type": "output_text", "text": "keep"}, map[string]any{"type": "output_text", "text": 7}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"function_call_output","name":"codex_apps_open_page","namespace":"agent","output":"keep"}]}`)
			body, err := sjson.SetBytes(body, "input.0."+tt.path, tt.value)
			require.NoError(t, err)
			got, names := normalizeCodexPageContext(body)
			require.Equal(t, body, got)
			require.Empty(t, names)
		})
	}
	for _, raw := range []string{
		`{"input":"hello"}`,
		`{"input":[{"role":"user","content":"codex_apps_open_page"}]}`,
		`{"input":[{"type":"function_call_output","name":"codex_apps_open_page","namespace":"agent","output":"keep","output":"different"}]}`,
		`{"input":[],"input":[{"type":"function_call_output","name":"codex_apps_open_page","namespace":"agent","output":"keep"}]}`,
		codexPageContextFixture + ` {}`,
	} {
		got, names := normalizeCodexPageContext([]byte(raw))
		require.Equal(t, raw, string(got))
		require.Empty(t, names)
	}
}

func TestNormalizeCodexPageContextReplayAndTextParts(t *testing.T) {
	for _, callID := range []any{nil, "", " \t"} {
		body := []byte(`{"previous_response_id":"resp_existing","large_number":9007199254740993,"input":[{"type":"function_call_output","namespace":"agent","name":"codex_apps_selected_page_part_2_of_3","id":"fco_old","output":[{"type":"output_text","text":"first\n"},{"type":"input_text","text":"second"}]},{"type":"function_call","call_id":"call_real","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_real","output":"unchanged"}]}`)
		body, err := sjson.SetBytes(body, "input.0.call_id", callID)
		require.NoError(t, err)
		// The raw prefilter must accept escaped JSON keys as well.
		body = bytes.ReplaceAll(body, []byte(`"type"`), []byte(`"\u0074ype"`))
		got, names := normalizeCodexPageContext(body)
		require.Len(t, names, 1)
		require.Equal(t, "9007199254740993", gjson.GetBytes(got, "large_number").Raw)
		require.Equal(t, "resp_existing", gjson.GetBytes(got, "previous_response_id").String())
		require.Equal(t, "input_text", gjson.GetBytes(got, "input.0.content.1.type").String())
		require.Equal(t, "first\n", gjson.GetBytes(got, "input.0.content.1.text").String())
		require.Equal(t, "second", gjson.GetBytes(got, "input.0.content.2.text").String())
		require.False(t, gjson.GetBytes(got, "input.0.id").Exists())
		for _, index := range []string{"1", "2"} {
			require.JSONEq(t, gjson.GetBytes(body, "input."+index).Raw, gjson.GetBytes(got, "input."+index).Raw)
		}
	}
}

func TestNormalizeCodexPageContextCoexistsWithDelegation(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","name":"codex_apps_open_page","namespace":"agent","output":"{}"},{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":"` + delegationEnvelope + `"}]}`)
	got, names := normalizeCodexPageContext(body)
	require.Len(t, names, 1)
	got, automation, delegation := normalizeCodexBootstraps(got)
	require.False(t, automation)
	require.True(t, delegation)
	require.Equal(t, "message", gjson.GetBytes(got, "input.0.type").String())
	require.Equal(t, "message", gjson.GetBytes(got, "input.1.type").String())
}

type codexPageContextUpstream struct {
	service.HTTPUpstream
	body []byte
}

func (u *codexPageContextUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	u.body = body
	response := `{"id":"resp_page_fixture","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"id":"msg_page","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`
	if gjson.GetBytes(body, "stream").Bool() {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n")),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(response)),
	}, nil
}

func TestOpenAIResponses_CodexPageContextReachesUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) { testCodexPageContextReachesUpstream(t, stream) })
	}
}

func testCodexPageContextReachesUpstream(t *testing.T, stream bool) {
	gin.SetMode(gin.TestMode)
	groupID := int64(4203)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
		ID: 9910, Name: "page-fixture", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		GroupIDs:    []int64{groupID},
		Credentials: map[string]any{"api_key": "fixture", "base_url": "https://api.example.test"},
		Extra:       map[string]any{"openai_passthrough": true},
	}}}
	upstream := &codexPageContextUpstream{}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(accountRepo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body, err := sjson.Set(codexPageContextFixture, "stream", stream)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 1803, GroupID: &groupID, AllowLongContext: true,
		User:  &service.User{ID: 1703, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1703})
	h.Responses(c)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.True(t, json.Valid(upstream.body))
	require.False(t, gjson.GetBytes(upstream.body, `input.#(type=="function_call_output")`).Exists())
	require.Equal(t, "user", gjson.GetBytes(upstream.body, "input.0.role").String())
	require.Equal(t, `{"page_id":null}`, gjson.GetBytes(upstream.body, "input.0.content.1.text").String())
	require.Equal(t, "hello", gjson.GetBytes(upstream.body, "input.2.content").String())
	require.Contains(t, recorder.Body.String(), "OK")
	if stream {
		require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
		require.Contains(t, recorder.Body.String(), "response.completed")
	}
}
