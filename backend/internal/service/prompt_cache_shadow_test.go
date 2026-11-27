package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func shadowTestService(t *testing.T) (*OpenAIGatewayService, *Account, *gin.Context, *[]map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 7, GroupID: ptrInt64(2)})
	var events []map[string]any
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{PromptCacheShadow: config.PromptCacheShadowConfig{Enabled: true, MinPrefixTokens: 32, PrefixTokensStep: 16}}}}
	svc.promptCacheShadow.emit = func(fields []zap.Field) {
		enc := zapcore.NewMapObjectEncoder()
		for _, f := range fields {
			f.AddTo(enc)
		}
		events = append(events, enc.Fields)
	}
	return svc, &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, c, &events
}

func shadowTestBody() []byte {
	body, _ := json.Marshal(map[string]any{"model": "gpt-6.2-astra", "input": strings.Repeat("PRIVATE text ", 300)})
	return body
}
func shadowTestResult() *OpenAIForwardResult {
	return &OpenAIForwardResult{RequestID: "rid_test", Usage: OpenAIUsage{InputTokens: 2048, OutputTokens: 25, CacheReadInputTokens: 1536, CacheReadKnown: true}}
}

func TestPromptCacheShadowNoAccountingMutationAndNoContent(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	body := shadowTestBody()
	before := bytes.Clone(body)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, body, "http")
	result := shadowTestResult()
	original := *result
	a.finish(result, nil)
	a.finish(result, nil)
	require.Equal(t, original, *result, "observer must not mutate billing usage")
	require.Equal(t, before, body)
	require.Len(t, *events, 1)
	row := (*events)[0]
	require.Equal(t, int64(1536), row["upstream_cache_read_tokens"])
	require.Equal(t, int64(0), row["simulated_cache_read_tokens"])
	require.Equal(t, "cold", row["decision"])
	require.Equal(t, true, row["committed"])
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE text")
	require.NotContains(t, string(raw), "input\":")
	ctx, a = svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, body, "http")
	a.finish(result, nil)
	require.Equal(t, "hit", (*events)[1]["decision"])
	require.Positive(t, (*events)[1]["simulated_cache_read_tokens"])
}

func TestPromptCacheShadowUnknownNotZeroAndFailedDoesNotWarm(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*OpenAIForwardResult)
		forwardErr error
		reason     string
		commit     bool
	}{
		{"missing cache", func(r *OpenAIForwardResult) { r.Usage.CacheReadKnown = false }, nil, "upstream_cache_unknown", true},
		{"missing usage", func(r *OpenAIForwardResult) { r.UsageUnavailable = true }, nil, "upstream_usage_unavailable", false},
		{"broken stream", func(r *OpenAIForwardResult) { r.streamReadIncomplete = true }, nil, "incomplete_request", false},
		{"failed terminal", func(r *OpenAIForwardResult) { r.UpstreamTerminalEvent = "response.failed" }, nil, "incomplete_request", false},
		{"disconnect", func(r *OpenAIForwardResult) { r.ClientDisconnect = true }, nil, "incomplete_request", false},
		{"read error", func(*OpenAIForwardResult) {}, errors.New("read failed"), "incomplete_request", false},
		{"counter overflow", func(r *OpenAIForwardResult) { r.Usage.CacheReadInputTokens = 99999 }, nil, "invalid_upstream_usage", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, account, c, events := shadowTestService(t)
			ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
			capturePromptCacheShadow(ctx, shadowTestBody(), "http")
			r := shadowTestResult()
			tc.mutate(r)
			a.finish(r, tc.forwardErr)
			row := (*events)[0]
			require.Equal(t, false, row["comparable"])
			require.Equal(t, tc.reason, row["skip_reason"])
			require.Equal(t, tc.commit, row["committed"])
			if !r.Usage.CacheReadKnown {
				require.Nil(t, row["upstream_cache_read_tokens"])
			}
			ctx, next := svc.beginPromptCacheShadow(context.Background(), c, account)
			capturePromptCacheShadow(ctx, shadowTestBody(), "http")
			if !tc.commit {
				require.Zero(t, next.attempt.MatchedPrefixTokens)
			}
		})
	}
}

func TestPromptCacheShadowHTTPUsesReplayReader(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	body := shadowTestBody()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/responses", bytes.NewReader(body))
	require.NoError(t, err)
	contentLength := r.ContentLength
	capturePromptCacheShadowHTTP(r)
	actual, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.Equal(t, body, actual)
	require.Equal(t, contentLength, r.ContentLength)
	a.finish(shadowTestResult(), nil)
	require.Equal(t, "upstream_wire", (*events)[0]["body_view"])
}

func TestPromptCacheShadowDisabledAndAccountAllowlist(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	for _, enabled := range []bool{false, true} {
		svc.cfg.Gateway.PromptCacheShadow.Enabled = enabled
		svc.cfg.Gateway.PromptCacheShadow.AccountIDs = []int64{999}
		ctx := context.Background()
		got, a := svc.beginPromptCacheShadow(ctx, c, account)
		require.Equal(t, ctx, got)
		require.Nil(t, a)
	}
	require.Empty(t, *events)
	svc.cfg.Gateway.PromptCacheShadow.AccountIDs = []int64{account.ID}
	_, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	require.NotNil(t, a)
}

func TestPromptCacheShadowOpenAIOnlyAndSecondaryExcluded(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, "deepseek", "composite", ""} {
		t.Run(platform, func(t *testing.T) {
			svc, account, c, events := shadowTestService(t)
			account.Platform = platform
			ctx := context.Background()
			got, attempt := svc.beginPromptCacheShadow(ctx, c, account)
			require.Equal(t, ctx, got)
			require.Nil(t, attempt)
			hooks := &OpenAIWSIngressHooks{}
			wrapped, cleanup := svc.wrapPromptCacheShadowHooks(c, account, hooks)
			cleanup()
			require.Same(t, hooks, wrapped)
			require.Empty(t, *events)
			require.Nil(t, svc.promptCacheShadow.engine)
		})
	}
	svc, account, c, _ := shadowTestService(t)
	svc.cfg.Site.Mode = site.Secondary
	_, attempt := svc.beginPromptCacheShadow(context.Background(), c, account)
	require.Nil(t, attempt)
	require.Nil(t, svc.promptCacheShadow.engine)
}

func TestPromptCacheShadowIndependentLogLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	svc, account, c, _ := shadowTestService(t)
	svc.promptCacheShadow.emit = nil
	ctx, attempt := svc.beginPromptCacheShadow(context.Background(), c, account)
	require.NotNil(t, attempt)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	r := shadowTestResult()
	before := *r
	attempt.finish(r, nil)
	svc.CloseOpenAIWSPool()
	require.Equal(t, before, *r)
	require.EqualValues(t, 1, svc.promptCacheShadow.log.Snapshot().WrittenEvents)
	files, err := filepath.Glob(filepath.Join(dir, "logs", "prompt-cache-shadow", "*.jsonl.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	_, attempt = svc.beginPromptCacheShadow(context.Background(), c, account)
	require.Nil(t, attempt)
}

func TestPromptCacheShadowLogInitFailureOnlyDisablesObserver(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	svc.promptCacheShadow.emit = nil
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, nil, 0600))
	svc.cfg.Gateway.PromptCacheShadow.LogDir = filepath.Join(file, "logs")
	ctx := context.Background()
	got, attempt := svc.beginPromptCacheShadow(ctx, c, account)
	require.Equal(t, ctx, got)
	require.Nil(t, attempt)
	svc.CloseOpenAIWSPool()
}

// Explicit local capacity experiment; no upstream or paid requests. Run alone
// without -race so instrumentation does not distort the memory measurements.
func TestPromptCacheShadowMemoryProfile(t *testing.T) {
	if os.Getenv("SUB2API_SHADOW_MEMORY_PROFILE") != "1" {
		t.Skip("explicit memory experiment")
	}
	runtime.GC()
	var baseline, warmed runtime.MemStats
	runtime.ReadMemStats(&baseline)
	codec, err := openAIInputTokensCodecForModel("gpt-6.2-sol")
	require.NoError(t, err)
	_, _, err = codec.Encode("initialization probe")
	require.NoError(t, err)
	runtime.GC()
	runtime.ReadMemStats(&warmed)
	t.Logf("shared tokenizer initialization: retained_heap_delta=%d bytes", int64(warmed.HeapAlloc)-int64(baseline.HeapAlloc))
	for _, size := range []int{100 << 10, 1 << 20, (2 << 20) - 1024} {
		for _, parallel := range []int{1, 2, 8} {
			svc, account, c, _ := shadowTestService(t)
			svc.cfg.Gateway.PromptCacheShadow.MinPrefixTokens = 1024
			svc.cfg.Gateway.PromptCacheShadow.PrefixTokensStep = 128
			svc.promptCacheShadow.emit = func([]zap.Field) {}
			text := strings.Repeat("prompt cache diagnostic words 中文 123456789 value. ", size/56)
			body, err := json.Marshal(map[string]any{"model": "gpt-6.2-sol", "input": text})
			require.NoError(t, err)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var peak atomic.Uint64
			peak.Store(before.HeapAlloc)
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(100 * time.Microsecond)
				defer ticker.Stop()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						var current runtime.MemStats
						runtime.ReadMemStats(&current)
						if current.HeapAlloc > peak.Load() {
							peak.Store(current.HeapAlloc)
						}
					}
				}
			}()
			var workers sync.WaitGroup
			var skipped atomic.Int64
			for i := 0; i < parallel; i++ {
				ctx, attempt := svc.beginPromptCacheShadow(context.Background(), c, account)
				workers.Add(1)
				go func() {
					defer workers.Done()
					r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/responses", bytes.NewReader(body))
					if err != nil {
						t.Error(err)
						return
					}
					capturePromptCacheShadowHTTP(r)
					if attempt.attempt.SkipReason != "" {
						skipped.Add(1)
					}
					attempt.finish(shadowTestResult(), nil)
				}()
			}
			workers.Wait()
			runtime.ReadMemStats(&after)
			if after.HeapAlloc > peak.Load() {
				peak.Store(after.HeapAlloc)
			}
			close(stop)
			<-done
			t.Logf("HTTP replay and calculation: parallel=%d body=%d bytes sampled_peak_heap_delta=%d bytes allocated=%d bytes skipped=%d", parallel, len(body), int64(peak.Load())-int64(before.HeapAlloc), after.TotalAlloc-before.TotalAlloc, skipped.Load())
		}
	}
}

func TestPromptCacheShadowWebSocketTurnsAndRetries(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	originalAfter := 0
	hooks, cleanup := svc.wrapPromptCacheShadowHooks(c, account, &OpenAIWSIngressHooks{AfterTurn: func(int, *OpenAIForwardResult, error) { originalAfter++ }})
	defer cleanup()
	first := shadowTestBody()
	second := bytes.Replace(first, []byte("PRIVATE"), []byte("CHANGED"), 1)
	hooks.ObserveUpstreamRequest(1, first)
	hooks.ObserveUpstreamRequest(1, second)
	r := shadowTestResult()
	before := *r
	hooks.AfterTurn(1, r, nil)
	require.Equal(t, before, *r)
	require.Len(t, *events, 1)
	require.Equal(t, int64(2), (*events)[0]["captures"])
	hooks.ObserveUpstreamRequest(2, second)
	hooks.AfterTurn(2, r, nil)
	require.Len(t, *events, 2)
	require.Equal(t, "hit", (*events)[1]["decision"])
	require.Equal(t, 2, originalAfter)
}

func TestPromptCacheShadowParallelObservers(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	var mu sync.Mutex
	rows := 0
	svc.promptCacheShadow.emit = func([]zap.Field) { mu.Lock(); rows++; mu.Unlock() }
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
			capturePromptCacheShadowValue(ctx, openAIWSPreparedJSON(shadowTestBody()))
			a.finish(shadowTestResult(), nil)
		}()
	}
	wg.Wait()
	require.Equal(t, 12, rows)
}

func TestPromptCacheShadowBusyDoesNotWaitOrAlterUsage(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	occupyShadowWorkers(t, svc)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	r := shadowTestResult()
	original := *r
	a.finish(r, nil)
	require.Equal(t, original, *r)
	require.Equal(t, "observer_busy", (*events)[0]["skip_reason"])
	require.Equal(t, false, (*events)[0]["comparable"])
	require.Equal(t, int64(1536), (*events)[0]["upstream_cache_read_tokens"])
}

func occupyShadowWorkers(t *testing.T, svc *OpenAIGatewayService) {
	t.Helper()
	workers := svc.promptCacheShadow.workers
	for i := 0; i < cap(workers); i++ {
		workers <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(workers); i++ {
			<-workers
		}
	})
}

func TestPromptCacheShadowEightComputationsAndNinthSkipsBeforeCopy(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() { once.Do(func() { close(release) }); workers.Wait() })
	attempts := make([]*promptCacheShadowAttempt, 8)
	body := []byte(`{"model":"gpt-6.2-sol","input":"hello"}`)
	for i := range attempts {
		_, a := svc.beginPromptCacheShadow(context.Background(), c, account)
		attempts[i] = a
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.capturePrepared("http", "upstream_wire", func() ([]byte, error) {
				entered <- struct{}{}
				<-release
				return body, nil
			})
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("all eight computations must be admitted")
		}
	}
	_, ninth := svc.beginPromptCacheShadow(context.Background(), c, account)
	loaded := false
	ninth.capturePrepared("http", "upstream_wire", func() ([]byte, error) { loaded = true; return body, nil })
	require.False(t, loaded)
	require.Equal(t, "observer_busy", ninth.attempt.SkipReason)
	once.Do(func() { close(release) })
	workers.Wait()
	for _, a := range attempts {
		require.Empty(t, a.attempt.SkipReason)
		a.finish(shadowTestResult(), nil)
	}
	ninth.finish(shadowTestResult(), nil)
}

type shadowCountingMarshaler struct{ calls *atomic.Int64 }

func (m shadowCountingMarshaler) MarshalJSON() ([]byte, error) {
	m.calls.Add(1)
	return shadowTestBody(), nil
}

func TestPromptCacheShadowBusy200RequestsDoNotCopyOrSerialize(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		t.Run(transport, func(t *testing.T) {
			svc, account, c, _ := shadowTestService(t)
			var events atomic.Int64
			svc.promptCacheShadow.emit = func([]zap.Field) { events.Add(1) }
			svc.promptCacheShadow.get(svc.cfg, account)
			occupyShadowWorkers(t, svc)
			var copies atomic.Int64
			var workers sync.WaitGroup
			for i := 0; i < 200; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
					if transport == "http" {
						r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/responses", nil)
						if err != nil {
							t.Error(err)
							return
						}
						r.GetBody = func() (io.ReadCloser, error) {
							copies.Add(1)
							return io.NopCloser(bytes.NewReader(shadowTestBody())), nil
						}
						capturePromptCacheShadowHTTP(r)
					} else {
						capturePromptCacheShadowValue(ctx, shadowCountingMarshaler{&copies})
					}
					if a.attempt.SkipReason != "observer_busy" {
						t.Errorf("unexpected decision %s", a.attempt.SkipReason)
					}
					a.finish(shadowTestResult(), nil)
				}()
			}
			workers.Wait()
			require.Zero(t, copies.Load(), "busy admission must precede replay/serialization")
			require.EqualValues(t, 200, events.Load(), "actual usage still produces diagnostic records")
		})
	}
}

func TestPromptCacheShadowUnknownInputAndOversizedLateModel(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	a.capture(make([]byte, (2<<20)+1), "http", "upstream_wire")
	a.finish(nil, errors.New("failed"))
	require.Equal(t, "unknown", (*events)[0]["model"])
	require.Nil(t, (*events)[0]["input_tokens"])
	require.Equal(t, "body_limit", (*events)[0]["skip_reason"])
	ctx, a = svc.beginPromptCacheShadow(ctx, c, account)
	require.NotNil(t, a, "a later attempt must replace the completed context")
	a.finish(nil, context.Canceled)
}

func TestPromptCacheShadowNativeAnthropicPresenceIsDiagnosticOnly(t *testing.T) {
	for _, raw := range []string{`{"usage":{"input_tokens":100,"cache_read_input_tokens":1536}}`, `{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":1536}}}`} {
		svc, account, c, events := shadowTestService(t)
		ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
		capturePromptCacheShadow(ctx, shadowTestBody(), "http")
		capturePromptCacheShadowAnthropicUsage(ctx, raw)
		r := shadowTestResult()
		r.Usage.CacheReadKnown = false
		before := *r
		a.finish(r, nil)
		require.Equal(t, before, *r, "diagnostic evidence must not change protection/billing fields")
		require.Equal(t, true, (*events)[0]["upstream_cache_known"])
		require.Equal(t, true, (*events)[0]["comparable"])
	}
}

func TestPromptCacheShadowWebSocketCleanupReleasesPending(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	hooks, cleanup := svc.wrapPromptCacheShadowHooks(c, account, nil)
	hooks.ObserveUpstreamRequest(1, shadowTestBody())
	cleanup()
	cleanup()
	require.Len(t, *events, 1)
	require.Nil(t, (*events)[0]["input_tokens"])
	require.Equal(t, false, (*events)[0]["committed"])
}
