package service

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPromptCacheShadowSnapshotSkipsUnchangedStateAndRetriesFailures(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	svc.promptCacheShadow.init(svc.cfg)
	path := filepath.Join(t.TempDir(), "cache.bin")
	s := &svc.promptCacheShadow
	require.NoError(t, s.saveState(path))
	first, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, s.saveState(path))
	unchanged, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(first, unchanged), "idle save must not replace the file")
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	a.finish(shadowTestResult(), nil)
	require.Error(t, s.saveState(filepath.Join(t.TempDir(), "missing", "cache.bin")))
	require.NoError(t, s.saveState(path))
	updated, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(first, updated), "a successful commit must be persisted")
	ctx, a = svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	a.finish(nil, context.Canceled)
	require.NoError(t, s.saveState(path))
	again, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(updated, again), "failed attempts must not force a snapshot")
}

func TestPromptCacheShadowNativeUsageDoesNotCaptureResponsesOutput(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	resp := &http.Response{Request: c.Request.WithContext(ctx)}
	capturePromptCacheShadowResponseUsage(resp, `{"type":"message_delta","output":[{"role":"assistant","content":"not a Responses completion"}],"usage":{"cache_read_input_tokens":1536}}`)
	require.False(t, a.attempt.OutputIndexed, "native usage frames must not parse Responses output")
	require.True(t, a.nativeCacheKnown)
	require.Equal(t, 1536, a.nativeCacheTokens)
	a.finish(shadowTestResult(), nil)
}

func TestPromptCacheShadowWebSocketObservesOnlyTerminalOutput(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done"} {
		t.Run(terminal, func(t *testing.T) {
			upstream := newStagedPassthroughConn()
			observed := make(chan string, 110)
			settled := make(chan int, 1)
			hooks := &OpenAIWSIngressHooks{
				ObserveUpstreamResponse: func(_ int, body []byte) { observed <- string(body) },
				AfterTurn:               func(_ int, _ *OpenAIForwardResult, _ error) { settled <- len(observed) },
			}
			server, done := startPassthroughLifecycleServer(t, context.Background(), newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream), passthroughLifecycleAccount(), hooks)
			defer server.Close()
			client := dialPassthroughLifecycleClient(t, server)
			defer func() { _ = client.CloseNow() }()
			requirePassthroughUpstreamWrite(t, upstream, time.Second)
			for range 100 {
				upstream.Send(`{"type":"response.output_text.delta","delta":"text"}`)
				_, err := readPassthroughLifecycleFrame(t, client, time.Second)
				require.NoError(t, err)
			}
			payload := `{"type":"` + terminal + `","response":{"id":"resp_shadow","output":[],"usage":{"input_tokens":2048,"output_tokens":100}}}`
			upstream.Send(payload)
			_, err := readPassthroughLifecycleFrame(t, client, time.Second)
			require.NoError(t, err)
			require.NoError(t, client.CloseNow())
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("relay did not finish")
			}
			require.Len(t, observed, 1, "delta frames must never invoke the output observer")
			require.Equal(t, 1, <-settled, "output must be captured before the attempt is settled")
			require.Equal(t, payload, <-observed)
		})
	}
}
