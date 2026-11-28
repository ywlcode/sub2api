package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
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
	capturePromptCacheShadowResponseUsage(resp, `{"type":"response.completed","response":{"output":[{"role":"assistant","content":"answer"}]}}`)
	require.True(t, a.attempt.OutputIndexed)
	a.finish(shadowTestResult(), nil)
}
