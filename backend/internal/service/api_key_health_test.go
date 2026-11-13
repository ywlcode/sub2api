package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type apiKeyHealthSample struct {
	success bool
	status  int
}

type apiKeyHealthTestCache struct {
	TempUnschedCache
	samples    []apiKeyHealthSample
	beginCalls int
	resetCalls int
	tripAt     int
	state      APIKeyHealthState
	stored     *TempUnschedState
}

func (c *apiKeyHealthTestCache) BeginAPIKeyHealthAttempt(context.Context, int64, APIKeyHealthConfig) (*APIKeyHealthState, error) {
	c.beginCalls++
	state := c.state
	return &state, nil
}
func (c *apiKeyHealthTestCache) RecordAPIKeyHealthResult(_ context.Context, _ int64, generation int64, success bool, status int, _ string) (*APIKeyHealthState, error) {
	if generation != c.state.Generation {
		return nil, nil
	}
	c.samples = append(c.samples, apiKeyHealthSample{success, status})
	if c.tripAt > 0 && len(c.samples) == c.tripAt {
		c.state = APIKeyHealthState{Generation: generation + 1, Tripped: true, TripCount: 1,
			WindowSize: 100, FailureThreshold: c.tripAt, Failures: c.tripAt, Samples: c.tripAt,
			TriggeredAtUnix: time.Now().Unix(), UntilUnix: time.Now().Add(15 * time.Minute).Unix(), StatusCode: status}
		state := c.state
		return &state, nil
	}
	return nil, nil
}
func (c *apiKeyHealthTestCache) ResetAPIKeyHealth(context.Context, int64) error {
	c.resetCalls++
	c.state.Generation++
	c.state.UntilUnix = 0
	return nil
}
func (c *apiKeyHealthTestCache) SetTempUnsched(_ context.Context, _ int64, state *TempUnschedState) error {
	c.stored = state
	return nil
}
func (c *apiKeyHealthTestCache) DeleteTempUnsched(context.Context, int64) error { return nil }

type apiKeyHealthTestRepo struct {
	AccountRepository
	account    *Account
	setCalls   int
	clearCalls int
	reason     string
	setErr     error
}

func (r *apiKeyHealthTestRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, reason string) error {
	r.setCalls++
	r.reason = reason
	return r.setErr
}
func (r *apiKeyHealthTestRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}
func (r *apiKeyHealthTestRepo) ClearTempUnschedulable(context.Context, int64) error {
	r.clearCalls++
	return nil
}
func (r *apiKeyHealthTestRepo) ClearModelRateLimits(context.Context, int64) error { return nil }
func (r *apiKeyHealthTestRepo) ClearRateLimit(context.Context, int64) error       { return nil }
func (r *apiKeyHealthTestRepo) ClearAntigravityQuotaScopes(context.Context, int64) error {
	return nil
}

type apiKeyHealthTestHTTP struct {
	DoFunc func(*http.Request) (*http.Response, error)
}

func (h apiKeyHealthTestHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.DoFunc(req)
}
func (h apiKeyHealthTestHTTP) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.DoFunc(req)
}

func newAPIKeyHealthFixture(t *testing.T) (*RateLimitService, *Account, *apiKeyHealthTestCache, *apiKeyHealthTestRepo) {
	t.Helper()
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{APIKeyHealthBreakerCredentialKey: map[string]any{"enabled": true}}}
	cache := &apiKeyHealthTestCache{state: APIKeyHealthState{Generation: 7}}
	repo := &apiKeyHealthTestRepo{account: account}
	return NewRateLimitService(repo, nil, nil, nil, cache), account, cache, repo
}

func TestAPIKeyHealthConfigDefaultsAndValidation(t *testing.T) {
	config := (&Account{Type: AccountTypeAPIKey, Credentials: map[string]any{
		APIKeyHealthBreakerCredentialKey: map[string]any{"enabled": true},
	}}).APIKeyHealthConfig()
	require.True(t, config.Enabled)
	require.Equal(t, 100, config.WindowSize)
	require.Equal(t, 10, config.FailureThreshold)
	require.False(t, (&Account{Type: AccountTypeOAuth}).APIKeyHealthConfig().Enabled)
	for _, value := range []any{true, "bad", map[string]any{"enabled": "true"},
		map[string]any{"window_size": 0}, map[string]any{"window_size": 10001},
		map[string]any{"failure_threshold": 0}, map[string]any{"window_size": 5, "failure_threshold": 6},
		map[string]any{"window_size": 100.5}} {
		err := ValidateAPIKeyHealthConfig(map[string]any{APIKeyHealthBreakerCredentialKey: value})
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "INVALID_API_KEY_HEALTH_CONFIG", infraerrors.Reason(err))
	}
}

func TestAPIKeyHealthHTTPRetriesAreIndependentSamples(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	statuses := []int{400, 502, 200}
	upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
		status := statuses[0]
		statuses = statuses[1:]
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	}}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
		require.NoError(t, err)
		resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", 42, 10)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, `{"ok":true}`, string(body))
		require.NoError(t, resp.Body.Close())
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, []apiKeyHealthSample{{false, 502}, {true, 200}}, cache.samples)
	require.Equal(t, 3, cache.beginCalls)
}

func TestAPIKeyHealthHTTPResponseClassification(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		status                  int
		include, success        bool
	}{
		{"ordinary 400", "application/json", `{"error":{"type":"invalid_request_error"}}`, 400, false, false},
		{"429", "application/json", `{"error":"overloaded"}`, 429, false, false},
		{"502", "text/html", "bad gateway", 502, true, false},
		{"credential policy unchanged", "application/json", `{"error":"invalid_api_key"}`, 401, false, false},
		{"200 error", "application/json", `{"error":{"message":"failed"}}`, 200, true, false},
		{"200 failed", "application/json", `{"status":"failed"}`, 200, true, false},
		{"200 malformed", "application/json", `{"output":`, 200, true, false},
		{"200 success", "application/json", `{"error":null,"output":[]}`, 200, true, true},
		{"large success", "application/json", `{"output":"` + strings.Repeat("a", 100000) + `"}`, 200, true, true},
		{"SSE completed", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", 200, true, true},
		{"large SSE completed", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"output\":\"" + strings.Repeat("a", 100000) + "\",\"status\":\"completed\"}}\n\n", 200, true, true},
		{"Responses output cap", "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", 200, false, false},
		{"completed output cap", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", 200, false, false},
		{"done output cap", "text/event-stream", "data: {\"type\":\"response.done\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", 200, false, false},
		{"content filtered", "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"content_filter\"}}}\n\n", 200, false, false},
		{"explicit cancellation", "text/event-stream", "data: {\"type\":\"response.cancelled\",\"response\":{\"status\":\"cancelled\"}}\n\n", 200, false, false},
		{"unknown incomplete", "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"upstream_broken\"}}}\n\n", 200, true, false},
		{"error with output cap", "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"error\":{\"message\":\"failed\"},\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", 200, true, false},
		{"nonterminal incomplete", "text/event-stream", "data: {\"type\":\"response.output_item.done\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", 200, true, false},
		{"large incomplete", "text/event-stream", `data: {"type":"response.incomplete","response":{"output":"` + strings.Repeat("a", 100000) + `","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n", 200, false, false},
		{"CC completed", "text/event-stream", "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n", 200, true, true},
		{"Claude completed", "text/event-stream", "data: {\"type\":\"message_stop\"}\n\n", 200, true, true},
		{"Gemini completed", "text/event-stream", "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n", 200, true, true},
		{"stream interrupted", "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n", 200, true, false},
		{"stream error before DONE", "text/event-stream", "data: {\"error\":{\"message\":\"failed\"}}\n\ndata: [DONE]\n\n", 200, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.status, Header: http.Header{"Content-Type": []string{tt.contentType}}, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
			}}
			req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
			require.NoError(t, err)
			resp, err := apiKeyHealthHTTP(upstream, limiter, account).DoWithTLS(req, "", 42, 10, nil)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tt.body, string(body))
			require.NoError(t, resp.Body.Close())
			if tt.include {
				require.Equal(t, []apiKeyHealthSample{{tt.success, tt.status}}, cache.samples)
			} else {
				require.Empty(t, cache.samples)
			}
		})
	}
}

func TestAPIKeyHealthHTTPTransportAndClientCancellation(t *testing.T) {
	for _, tt := range []struct {
		name            string
		err             error
		cancel, include bool
	}{
		{"upstream timeout", &url.Error{Op: "Post", URL: "https://upstream.example", Err: os.ErrDeadlineExceeded}, false, false},
		{"connection EOF", io.EOF, false, false},
		{"local rejection", errors.New("local admission rejected"), false, false},
		{"client cancel", context.Canceled, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) { return nil, tt.err }}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", nil)
			require.NoError(t, err)
			_, err = apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", 42, 10)
			require.Error(t, err)
			if tt.include {
				require.Len(t, cache.samples, 1)
			} else {
				require.Empty(t, cache.samples)
			}
		})
	}
}

func TestAPIKeyHealthHTTPTripPersistsAndStopsRetries(t *testing.T) {
	limiter, account, cache, repo := newAPIKeyHealthFixture(t)
	cache.tripAt = 1
	sends := 0
	upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
		sends++
		return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("error"))}, nil
	}}
	req, err := http.NewRequest(http.MethodPost, "https://upstream.example", nil)
	require.NoError(t, err)
	resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", 42, 10)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, repo.setCalls)
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.reason), &state))
	require.Equal(t, APIKeyHealthBreakerReason, state.MatchedKeyword)
	require.Equal(t, TempUnschedScopeAccount, state.Scope)
	require.Equal(t, 100, state.TriggerWindowRequests)
	require.Equal(t, 1, state.BreakerTripCount)
	require.EqualValues(t, 900, state.UntilUnix-state.TriggeredAtUnix)
	_, err = apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", 42, 10)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, 1, sends)
	require.Len(t, cache.samples, 1)
	require.Nil(t, account.TempUnschedulableUntil, "shared account snapshots must not be mutated")
}

func TestAPIKeyHealthSuccessfulProbePreservesCooldownAndManualRecoveryClearsIt(t *testing.T) {
	limiter, account, cache, repo := newAPIKeyHealthFixture(t)
	until := time.Now().Add(15 * time.Minute)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = `{"matched_keyword":"api_key_health_breaker"}`
	result, err := limiter.RecoverAccountAfterSuccessfulTest(context.Background(), 42)
	require.NoError(t, err)
	require.False(t, result.ClearedRateLimit)
	require.Zero(t, repo.clearCalls)
	require.Zero(t, cache.resetCalls)
	require.NoError(t, limiter.ClearTempUnschedulable(context.Background(), 42))
	require.Equal(t, 1, repo.clearCalls)
	require.Equal(t, 1, cache.resetCalls)
}

func TestAPIKeyHealthSuccessfulProbePreservesRedisCooldownUnderAnotherReason(t *testing.T) {
	limiter, account, cache, repo := newAPIKeyHealthFixture(t)
	until := time.Now().Add(time.Hour)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = `{"matched_keyword":"another_rule"}`
	cache.state.UntilUnix = time.Now().Add(15 * time.Minute).Unix()
	result, err := limiter.RecoverAccountAfterSuccessfulTest(context.Background(), 42)
	require.NoError(t, err)
	require.False(t, result.ClearedRateLimit)
	require.Zero(t, repo.clearCalls)
	require.Zero(t, cache.resetCalls)
	require.Empty(t, cache.samples, "probes are not ordinary successful traffic")
}

func TestAPIKeyHealthSuccessfulProbeAfterExpiryDoesNotResetEscalation(t *testing.T) {
	limiter, account, cache, repo := newAPIKeyHealthFixture(t)
	until := time.Now().Add(-time.Minute)
	account.TempUnschedulableUntil = &until
	account.TempUnschedulableReason = `{"matched_keyword":"api_key_health_breaker"}`
	cache.state.TripCount = 2
	result, err := limiter.RecoverAccountAfterSuccessfulTest(context.Background(), 42)
	require.NoError(t, err)
	require.True(t, result.ClearedRateLimit)
	require.Equal(t, 1, repo.clearCalls)
	require.Zero(t, cache.resetCalls)
	require.Equal(t, 2, cache.state.TripCount)
}

type apiKeyHealthReadFunc func([]byte) (int, error)

func (f apiKeyHealthReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestAPIKeyHealthHTTPTimeoutsDuringSendAndStream(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, cause := range []error{&openAIHTTPPhaseTimeout{phase: "pre_output_budget"}, context.DeadlineExceeded, context.Canceled} {
			t.Run(fmt.Sprintf("stream=%v/cause=%v", streaming, cause), func(t *testing.T) {
				limiter, account, cache, _ := newAPIKeyHealthFixture(t)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				upstream := apiKeyHealthTestHTTP{DoFunc: func(r *http.Request) (*http.Response, error) {
					httptrace.ContextClientTrace(r.Context()).WroteRequest(httptrace.WroteRequestInfo{})
					if !streaming {
						cancel(cause)
						return nil, ctx.Err()
					}
					body := io.NopCloser(apiKeyHealthReadFunc(func([]byte) (int, error) {
						cancel(cause)
						return 0, ctx.Err()
					}))
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
				}}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", nil)
				require.NoError(t, err)
				resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
				if streaming {
					require.NoError(t, err)
					_, err = io.ReadAll(resp.Body)
					require.NoError(t, resp.Body.Close())
				}
				require.Error(t, err)
				if errors.Is(cause, context.Canceled) {
					require.Empty(t, cache.samples)
				} else {
					require.Len(t, cache.samples, 1)
					require.False(t, cache.samples[0].success)
				}
			})
		}
	}
}

func TestAPIKeyHealthHTTPFirstOutputHeaderTimeoutCountsFailure(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	ctx, guard := newOpenAIFirstOutputHeaderGuard(context.Background(), func() {}, time.Now().Add(10*time.Millisecond))
	defer guard.close()
	upstream := apiKeyHealthTestHTTP{DoFunc: func(req *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", nil)
	require.NoError(t, err)
	_, err = apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []apiKeyHealthSample{{false, 0}}, cache.samples)
}

func TestAPIKeyHealthHTTPSemanticOutputTimeoutCountsBeforeCleanupCancellation(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", nil)
	require.NoError(t, err)
	resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
	require.NoError(t, err)
	resp.Body = &openAIRequestContextReadCloser{ReadCloser: resp.Body, cleanup: cancel}
	svc := &OpenAIGatewayService{rateLimitService: limiter, cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now().Add(-2*time.Second), "model", "model")
	require.Error(t, err)
	require.Equal(t, []apiKeyHealthSample{{false, 200}}, cache.samples)
}

func TestAPIKeyHealthHTTPCompletedStreamCountsBeforeCleanupCancellation(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))}, nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", nil)
	require.NoError(t, err)
	resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
	require.NoError(t, err)
	buf := make([]byte, 128)
	_, err = resp.Body.Read(buf) // Deliberately stop at the terminal without EOF.
	require.NoError(t, err)
	cancel()
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []apiKeyHealthSample{{true, 200}}, cache.samples)
}

func TestAPIKeyHealthHTTPDetachedClientCancellationIsExcluded(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstreamCtx, release := detachUpstreamContext(clientCtx)
	defer release()
	upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(apiKeyHealthReadFunc(func([]byte) (int, error) {
				cancelClient()
				return 0, io.ErrUnexpectedEOF
			}))}, nil
	}}
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, "https://upstream.example", nil)
	require.NoError(t, err)
	resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NoError(t, resp.Body.Close())
	require.Empty(t, cache.samples)
}
