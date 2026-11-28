package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromptCacheShadowCoverageOpaqueAndCrossAccount(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	c.Set("api_key", &APIKey{ID: 7, UserID: 88})
	body := []byte(`{"model":"gpt-6.2-sol","input":[{"type":"compaction","encrypted_content":"PRIVATE-OPAQUE-CONTEXT"}]}`)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, body, "http")
	r := shadowTestResult()
	r.Usage.InputTokens = 90000
	a.finish(r, nil)
	require.Equal(t, true, (*events)[0]["comparable"])
	require.Equal(t, int64(1), (*events)[0]["opaque_blocks"])
	other := *account
	other.ID++
	c.Set("api_key", &APIKey{ID: 8, UserID: 88})
	ctx, a = svc.beginPromptCacheShadow(context.Background(), c, &other)
	capturePromptCacheShadow(ctx, body, "http")
	r.Usage.CacheReadInputTokens = 0
	a.finish(r, nil)
	require.Equal(t, int64(90000), (*events)[1]["simulated_cache_read_tokens"])
	require.Zero(t, (*events)[1]["matched_prefix_tokens"])
	c.Set("api_key", &APIKey{ID: 9, UserID: 89})
	ctx, a = svc.beginPromptCacheShadow(context.Background(), c, &other)
	capturePromptCacheShadow(ctx, body, "http")
	a.finish(r, nil)
	require.Zero(t, (*events)[2]["simulated_cache_read_tokens"])
	raw, err := json.Marshal(*events)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE-OPAQUE-CONTEXT")
}

func TestPromptCacheShadowCoverageOutputAndPersistentRestart(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	dir := filepath.Join(t.TempDir(), "logs")
	svc.cfg.Gateway.PromptCacheShadow.LogDir = dir
	svc.promptCacheShadow.emit = nil
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, []byte(`{"model":"gpt-6.2-sol","input":[{"role":"user","content":"hello"}]}`), "http")
	capturePromptCacheShadowOutput(ctx, `{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":"world"}]}}`)
	require.True(t, a.attempt.OutputIndexed)
	r := shadowTestResult()
	a.finish(r, nil)
	svc.promptCacheShadow.close()
	require.FileExists(t, filepath.Join(dir, "state", "cache.bin"))
	second, _, _, _ := shadowTestService(t)
	second.cfg.Gateway.PromptCacheShadow.LogDir = dir
	second.promptCacheShadow.emit = nil
	defer second.promptCacheShadow.close()
	ctx, next := second.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, []byte(`{"model":"gpt-6.2-sol","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"world"},{"role":"user","content":"next"}]}`), "http")
	require.True(t, second.promptCacheShadow.stateRestored)
	got, _ := next.attempt.Compare(2100)
	require.GreaterOrEqual(t, got, 2073)
	next.finish(r, nil)
}

func TestPromptCacheShadowCoverageUsesWireHTTPOutput(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	resp := httptest.NewRecorder().Result()
	resp.Request = c.Request
	capturePromptCacheShadowHTTPOutput(resp, []byte(`{"type":"response.completed","response":{"output":[{"role":"assistant","content":"answer"}]}}`))
	require.True(t, a.attempt.OutputIndexed)
	a.finish(shadowTestResult(), nil)
}

func TestPromptCacheShadowOutputWithoutRegionalMeterContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		svc, account, c, _ := shadowTestService(t)
		ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
		capturePromptCacheShadow(ctx, shadowTestBody(), "http")
		// The incoming request keeps its original context when regional metering
		// is disabled. The outgoing wire request carries the observer context.
		require.Nil(t, c.Request.Context().Value(promptCacheShadowContextKey{}))
		body := `{"id":"resp_test","output":[{"role":"assistant","content":"answer"}],"usage":{"input_tokens":2048,"output_tokens":25,"input_tokens_details":{"cached_tokens":1536}}}`
		contentType := "application/json"
		if stream {
			body = "data: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
			contentType = "text/event-stream"
		}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)}
		_, err := svc.handleNonStreamingResponsePassthrough(ctx, resp, c, account, "gpt-6.2-astra", "gpt-6.2-astra")
		require.NoError(t, err)
		require.True(t, a.attempt.OutputIndexed, "wire output must not depend on regional metering")
		a.finish(shadowTestResult(), nil)
	}
}

func TestPromptCacheShadowLogsProtectedAnchorAndOpaqueHistory(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	body := []byte(`{"model":"gpt-6.2-sol","input":[{"type":"compaction","encrypted_content":"PRIVATE-ANCHOR-CIPHER"}]}`)
	for range 2 {
		ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
		capturePromptCacheShadow(ctx, body, "http")
		a.finish(shadowTestResult(), nil)
	}
	require.Len(t, *events, 2)
	first, second := (*events)[0], (*events)[1]
	require.EqualValues(t, 100000, first["measured_anchor_capacity"])
	require.EqualValues(t, 10000, first["opaque_history_capacity"])
	require.EqualValues(t, 1, first["opaque_unseen_blocks"])
	require.Equal(t, "not_found", first["measured_anchor_status"])
	require.EqualValues(t, 1, second["opaque_known_blocks"])
	require.EqualValues(t, 0, second["opaque_unseen_blocks"])
	require.Equal(t, "hit", second["measured_anchor_status"])
	raw, err := json.Marshal(*events)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE-ANCHOR-CIPHER")
}
