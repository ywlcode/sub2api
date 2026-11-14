package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestConfigInvalidIdentityStormKeepsHotEntries(t *testing.T) {
	var cache requestConfigCache[int, string]
	var loads atomic.Int64
	load := func(context.Context) (string, int, error) { loads.Add(1); return "trusted-config", 14, nil }
	_, err := cache.get(context.Background(), 1, load)
	require.NoError(t, err)
	for key := 2; key <= 10000; key++ {
		_, err = cache.get(context.Background(), key, func(context.Context) (string, int, error) { return "", 0, errors.New("unknown key") })
		require.Error(t, err)
	}
	_, err = cache.get(context.Background(), 1, load)
	require.NoError(t, err)
	require.Equal(t, int64(1), loads.Load())
	require.Len(t, cache.versions, 1)
	require.Empty(t, cache.active)
}

func TestRequestConfigCancelledLoaderCannotPublishIntoReusedKey(t *testing.T) {
	var cache requestConfigCache[int, string]
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cache.get(ctx, 1, func(context.Context) (string, int, error) {
			close(started)
			<-release
			close(finished)
			return "stale", 5, nil
		})
		done <- err
	}()
	<-started
	cache.invalidate(func(key int) bool { return key == 1 })
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	value, err := cache.get(context.Background(), 1, func(context.Context) (string, int, error) { return "current", 7, nil })
	require.NoError(t, err)
	require.Equal(t, "current", value)
	close(release)
	<-finished
	value, err = cache.get(context.Background(), 1, func(context.Context) (string, int, error) { t.Error("hot entry was lost"); return "wrong", 5, nil })
	require.NoError(t, err)
	require.Equal(t, "current", value)
}

func TestAPIKeyHealth500AttributionIndependentOfBodyConsumer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		failure    bool
	}{
		{"generic invalid request", `{"error":{"type":"invalid_request_error","message":"Internal failure"}}`, true},
		{"context is a client error", `{"error":{"type":"invalid_request_error","code":"context_length_exceeded"}}`, false},
		{"quota overrides broad type", `{"error":{"type":"invalid_request_error","code":"insufficient_quota"}}`, true},
		{"cyber is neutral", `{"error":{"code":"cyber_policy"}}`, false},
	} {
		for _, readBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/read=%t", tc.name, readBody), func(t *testing.T) {
				limiter, account, cache, _ := newAPIKeyHealthFixture(t)
				upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				}}
				req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid/responses", nil)
				require.NoError(t, err)
				resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
				require.NoError(t, err)
				if readBody {
					got, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					require.Equal(t, tc.body, string(got))
				}
				require.NoError(t, resp.Body.Close())
				if tc.failure {
					require.Equal(t, []apiKeyHealthSample{{false, 500}}, cache.samples)
				} else {
					require.Empty(t, cache.samples)
				}
			})
		}
	}
}

func TestAPIKeyHealthQuotaStreamIsAccountError(t *testing.T) {
	for _, kind := range []string{"insufficient_quota", "invalid_request_error"} {
		t.Run(kind, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"type":"response.failed","response":{"error":{"type":%q,"code":"insufficient_quota","message":"Insufficient balance"}}}`, kind))
			require.Equal(t, 429, openAIStreamFailedEventSemanticStatus(payload, "Insufficient balance"))
			require.False(t, apiKeyHealthClientErrorJSON(payload))
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(payload) + "\n\n"))}, nil
			}}
			req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid/responses", nil)
			require.NoError(t, err)
			resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []apiKeyHealthSample{{false, 200}}, cache.samples)
		})
	}
}

func TestOpenAIStreamSpecificAccountErrorsOverrideBroadInvalidRequestType(t *testing.T) {
	for code, status := range map[string]int{"insufficient_quota": 429, "quota_exceeded": 429, "invalid_api_key": 401, "invalid_argument": 400, "context_length_exceeded": 400} {
		payload := []byte(fmt.Sprintf(`{"error":{"type":"invalid_request_error","code":%q}}`, code))
		require.Equal(t, status, openAIStreamFailedEventSemanticStatus(payload, ""), code)
	}
}
