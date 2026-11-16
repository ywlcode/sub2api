//go:build unit

package service

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyHealthMultilineSSEMatchesConsumer(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          []apiKeyHealthSample
	}{
		{"successful_terminal", "event: message_stop\ndata: {\ndata: \"type\":\"message_stop\"\ndata: }\n\n", []apiKeyHealthSample{{true, 200}}},
		{"client_context_error", "event: error\ndata: {\ndata: \"type\":\"error\",\ndata: \"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\"}\ndata: }\n\n", nil},
		{"upstream_error", "event:error\ndata: {\ndata: \"type\":\"error\",\ndata: \"error\":{\"type\":\"server_error\"}\ndata: }\n\n", []apiKeyHealthSample{{false, 200}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(tc.payload)))}, nil
			}}
			resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(httptest.NewRequest(http.MethodPost, "https://example.invalid/v1/messages", nil), "", account.ID, 1)
			require.NoError(t, err)
			defer resp.Body.Close()
			pump := newAnthropicNativeLinePump(bufio.NewScanner(resp.Body), time.Second)
			defer pump.stop()
			payload, err := pump.nextData(4096)
			require.NoError(t, err)
			require.Contains(t, payload, `"type"`)
			_, err = pump.nextData(4096)
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, tc.want, cache.samples)
		})
	}
}

func TestAPIKeyHealthSSEEventBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          []apiKeyHealthSample
	}{
		{"CRLF_and_comments", ": heartbeat\r\nevent: message_stop\r\ndata: {\r\nid: 7\r\ndata: \"type\":\"message_stop\"\r\ndata: }\r\n\r\n", []apiKeyHealthSample{{true, 200}}},
		{"EOF_without_blank_line", "data: {\ndata: \"type\":\"message_stop\"\ndata: }", []apiKeyHealthSample{{true, 200}}},
		{"event_name_resets", "event:error\n\nevent: message_stop\ndata: {\ndata: \"type\":\"message_stop\"\ndata: }\n\n", []apiKeyHealthSample{{true, 200}}},
		{"invalid_JSON", "data: {\ndata: broken\ndata: }\n\n", []apiKeyHealthSample{{false, 200}}},
		{"DONE", "data: [DONE]\n\n", []apiKeyHealthSample{{true, 200}}},
		{"legacy_missing_separator", "data: {\"type\":\"message_start\"}\ndata: {\"type\":\"message_stop\"}\ndata: [DONE]\n\n", []apiKeyHealthSample{{true, 200}}},
		{"many_short_data_lines", "data: {\ndata: \"type\":\"message_stop\",\ndata: \"output\":[\n" + strings.Repeat("data: \"padding\",\n", 15000) + "data: \"end\"]}\n\n", []apiKeyHealthSample{{true, 200}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.payload))}, nil
			}}
			resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(httptest.NewRequest(http.MethodPost, "https://example.invalid/v1/messages", nil), "", account.ID, 1)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.payload, string(body), "health inspection must preserve every response byte")
			require.Equal(t, tc.want, cache.samples)
			observed := resp.Body.(*apiKeyHealthBody)
			require.LessOrEqual(t, len(observed.eventData), priorityAttemptInspectBytes)
			require.LessOrEqual(t, len(observed.buf), priorityAttemptInspectBytes)
		})
	}
}

func TestAPIKeyHealthSSEWaitsForCompleteEvent(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	observation, err := beginAPIKeyHealthAttempt(context.Background(), limiter, account)
	require.NoError(t, err)
	const firstLine = "data: {\n"
	body := &apiKeyHealthBody{ReadCloser: io.NopCloser(strings.NewReader(firstLine + "data: \"type\":\"message_stop\"\ndata: }\n\n")), observation: observation, status: 200, stream: true}
	defer body.Close()
	_, err = io.ReadFull(body, make([]byte, len(firstLine)))
	require.NoError(t, err)
	require.Empty(t, cache.samples)
	_, err = io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, []apiKeyHealthSample{{true, 200}}, cache.samples)
}

// Real HTTP transport plus the non-OpenAI phase guard: receiving headers stops
// upload/header timers, and the success-stream idle timeout does not cover the
// gateway error reader. Exercise both prefetch and reads beyond its byte cap.
func TestAPIKeyHealthStalledHTTPErrorReadIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name, padding, code string
		cancel              bool
	}{
		{name: "stalled_prefetch", code: "server_error"},
		{name: "stalled_after_prefetch", padding: strings.Repeat("x", priorityAttemptInspectBytes+100), code: "server_error"},
		{name: "complete_client_error_without_EOF", code: "context_length_exceeded"},
		{name: "client_cancellation", code: "server_error", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := `{"error":{"code":"` + tc.code + `","message":"upstream unavailable` + tc.padding + `"}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, payload)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			defer srv.CloseClientConnections()
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			account.Platform = PlatformDeepseek
			cfg := &config.Config{}
			cfg.Gateway.UpstreamUploadTimeoutSeconds = 10
			cfg.Gateway.ResponseHeaderTimeout = 10
			cfg.Gateway.StreamDataIntervalTimeout = 1
			client := srv.Client()
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses", nil)
			require.NoError(t, err)
			start := time.Now()
			resp, err := gatewayHTTPUpstream(apiKeyHealthTestHTTP{DoFunc: client.Do}, limiter, account, cfg).Do(req, "", account.ID, 1)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			done := make(chan []byte, 1)
			go func() { done <- (&OpenAIGatewayService{cfg: cfg}).readUpstreamErrorBody(resp) }()
			deadline := start.Add(apiKeyHealthErrorBodyTimeout + 2*time.Second)
			if tc.cancel {
				cancel()
				deadline = time.Now().Add(time.Second)
			}
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case body := <-done:
				require.Equal(t, payload, string(body), "retain bytes received before the deadline/cancellation")
				t.Logf("HTTP 503 error reader finished in %s", time.Since(start))
			case <-timer.C:
				_ = resp.Body.Close()
				<-done
				t.Fatal("HTTP error body still blocked after its read deadline")
			}
			require.NoError(t, resp.Body.Close())
			if tc.cancel || tc.code == "context_length_exceeded" {
				require.Empty(t, cache.samples)
			} else {
				require.Equal(t, []apiKeyHealthSample{{false, 503}}, cache.samples)
			}
		})
	}
}

func TestAPIKeyHealthErrorConcurrentReadAndClose(t *testing.T) {
	limiter, account, cache, _ := newAPIKeyHealthFixture(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: reader}, nil
	}}
	resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(httptest.NewRequest(http.MethodPost, "https://example.invalid/v1/responses", nil), "", account.ID, 1)
	require.NoError(t, err)
	defer resp.Body.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(resp.Body)
		close(done)
	}()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = resp.Body.Close() }()
	}
	wg.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close failed to unblock error-body read")
	}
	require.Equal(t, []apiKeyHealthSample{{false, 503}}, cache.samples)
}
