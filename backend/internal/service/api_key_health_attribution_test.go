package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Repeated client mistakes must not remove a shared account from the pool.
func TestAPIKeyHealthClientMistakesNeverTrip(t *testing.T) {
	for _, tc := range []struct {
		name, method, content, body string
		status                      int
	}{
		{"misclassified client error", "POST", "application/json", `{"error":{"code":"context_length_exceeded"}}`, 500},
		{"bad request", "POST", "application/json", `{"error":{"type":"invalid_request_error"}}`, 400},
		{"upload timeout", "POST", "application/json", `{}`, 408},
		{"limited", "POST", "application/json", `{}`, 429},
		{"context SSE", "POST", "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"code\":\"context_length_exceeded\"}\n\n", 200},
		{"Anthropic SSE", "POST", "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\"}}\n\n", 200},
		{"Responses SSE", "POST", "text/event-stream", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\"}}}\n\n", 200},
		{"video poll", "GET", "application/json", `{"status":"failed"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter, account, cache, repo := newAPIKeyHealthFixture(t)
			cache.tripAt = 10
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.content}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}}
			for i := 0; i < 20; i++ {
				req, err := http.NewRequestWithContext(context.Background(), tc.method, "https://upstream.example", nil)
				require.NoError(t, err)
				resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
				require.NoError(t, err)
				// One byte per read covers event/data arriving in different packets.
				var got strings.Builder
				buf := make([]byte, 1)
				for {
					n, e := resp.Body.Read(buf)
					_, _ = got.Write(buf[:n])
					if e != nil {
						require.ErrorIs(t, e, io.EOF)
						break
					}
				}
				require.Equal(t, tc.body, got.String())
				require.NoError(t, resp.Body.Close())
				recordAPIKeyHealthResponseFailure(context.Background(), resp, "upstream rejected request")
			}
			require.Empty(t, cache.samples)
			require.Zero(t, repo.setCalls)
		})
	}
}

func TestAPIKeyHealthWebSocketClientErrorIsNeutral(t *testing.T) {
	limiter, account, cache, repo := newAPIKeyHealthFixture(t)
	cache.tripAt = 10
	tracker := newAPIKeyHealthWSTracker(context.Background(), limiter, account)
	for i := 0; i < 20; i++ {
		require.NoError(t, tracker.beforeSend())
		tracker.receive([]byte(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}}}`))
	}
	require.Empty(t, cache.samples)
	require.Zero(t, repo.setCalls)
}
