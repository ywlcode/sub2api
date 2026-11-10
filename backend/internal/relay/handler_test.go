package relay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, upstream http.HandlerFunc, change func(*Config)) (*Handler, *httptest.Server) {
	t.Helper()
	u := httptest.NewServer(upstream)
	t.Cleanup(u.Close)
	c := Config{Key: "relay-fixture", UpstreamKey: "upstream-fixture", UpstreamURL: u.URL + "/api/v3", MaxBodyBytes: 1 << 20, UploadTimeout: time.Second, ResponseHeaderTimeout: time.Second}
	if change != nil {
		change(&c)
	}
	h, err := NewHandler(c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return h, s
}

func call(t *testing.T, s *httptest.Server, body []byte, compressed bool) *http.Response {
	t.Helper()
	return callPath(t, s, "/v1/responses", body, compressed)
}

func callPath(t *testing.T, s *httptest.Server, path string, body []byte, compressed bool) *http.Response {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer relay-fixture")
	r.Header.Set("Content-Type", "application/json")
	if compressed {
		r.Header.Set("Content-Encoding", "gzip")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func zipped(t *testing.T, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := io.WriteString(gz, body); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRelayBytePreservingRequestAndStream(t *testing.T) {
	responsesBody := `{
  "model":"deepseek", "stream":true, "seed":9007199254740993,
  "reasoning":{"effort":"off"}, "thinking":{"type":"disabled"},
  "input":[
    {"role":"user","content":"<thinking>literal</thinking>\n\u4e2d", "internal_chat_message_metadata_passthrough":{"keep":true}},
    {"type":"reasoning","content":[{"type":"reasoning_text","text":"history"}]},
    {"role":"assistant","content":[{"type":"output_text","text":"answer","logprobs":[{"token":"x","logprob":-0.1}]}]}
  ]
}
`
	chatBody := `{
  "model":"deepseek", "stream":true, "seed":9007199254740993,
  "reasoning":{"effort":"off"}, "thinking":{"type":"enabled"},
  "messages":[
    {"role":"user","content":"<thinking>literal</thinking>\n\u4e2d"},
    {"role":"assistant","reasoning_content":"history","content":"answer", "internal_chat_message_metadata_passthrough":{"keep":true}}
  ]
}
`
	responsesStream := "event: response.reasoning_text.delta\r\ndata: {\"delta\":\"<thinking>history</thinking>\"}\r\n\r\nevent: response.output_text.delta\ndata: {\"delta\":\"<tag>\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	chatStream := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"<thinking>history</thinking>\"}}]}\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"<tag>\"}}]}\n\ndata: [DONE]\n\n"
	for _, endpoint := range []struct {
		path, upstreamPath, body, stream string
	}{
		{"/responses", "/responses", responsesBody, responsesStream},
		{"/v1/responses", "/responses", responsesBody, responsesStream},
		{"/chat/completions", "/chat/completions", chatBody, chatStream},
		{"/v1/chat/completions", "/chat/completions", chatBody, chatStream},
	} {
		for _, encoding := range []string{"plain", "gzip"} {
			t.Run(endpoint.path+"/"+encoding, func(t *testing.T) {
				seen := make(chan []byte, 1)
				h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v3"+endpoint.upstreamPath || r.Header.Get("Authorization") != "Bearer upstream-fixture" || r.Header.Get("Content-Encoding") != "" {
						t.Error("incorrect upstream URL/auth/encoding")
					}
					if r.ContentLength != int64(len(endpoint.body)) || len(r.TransferEncoding) != 0 {
						t.Error("decoded body length not reflected in upstream request")
					}
					b, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					seen <- b
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("X-Request-Id", "ark-fixture")
					_, _ = io.WriteString(w, endpoint.stream)
				}, nil)
				body := []byte(endpoint.body)
				if encoding == "gzip" {
					body = zipped(t, endpoint.body)
				}
				res := callPath(t, s, endpoint.path, body, encoding == "gzip")
				out, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				if res.StatusCode != http.StatusOK || res.Header.Get("X-Request-Id") != "ark-fixture" || string(out) != endpoint.stream {
					t.Fatalf("response changed: %d %q", res.StatusCode, out)
				}
				if got := <-seen; !bytes.Equal(got, []byte(endpoint.body)) {
					t.Errorf("request bytes changed: %q", got)
				}
				waitCompleted(t, h, 1)
				snap := h.Timings.Snapshot()
				if snap.Errors != 0 || snap.Samples != 1 || snap.Upload.AverageMS == nil || snap.Preparation.AverageMS == nil || snap.FirstToken.AverageMS == nil {
					t.Fatalf("bad timings: %+v", snap)
				}
			})
		}
	}
}

func TestRelayLeavesRequestValidationToUpstream(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, encoding := range []string{"plain", "gzip"} {
			for _, test := range []struct{ name, body string }{
				{"empty", ""},
				{"multiple values", "{} {}"},
				{"malformed JSON", "{\"input\":"},
				{"non UTF-8", "{\"input\":\"\xff\"}"},
				{"non object", "[1,2,3]"},
			} {
				t.Run(endpoint+"/"+encoding+"/"+test.name, func(t *testing.T) {
					seen := make(chan []byte, 1)
					upstreamError := "{\"error\":{\"message\":\"upstream validation error\"}}\n"
					h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						seen <- body
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, upstreamError)
					}, nil)
					body := []byte(test.body)
					if encoding == "gzip" {
						body = zipped(t, test.body)
					}
					res := callPath(t, s, endpoint, body, encoding == "gzip")
					out, err := io.ReadAll(res.Body)
					if err != nil {
						t.Fatal(err)
					}
					if res.StatusCode != http.StatusBadRequest || string(out) != upstreamError {
						t.Fatalf("upstream error changed: %d %q", res.StatusCode, out)
					}
					if got := <-seen; !bytes.Equal(got, []byte(test.body)) {
						t.Errorf("opaque request bytes changed: %q", got)
					}
					waitCompleted(t, h, 1)
					if snap := h.Timings.Snapshot(); snap.Errors != 1 || snap.Samples != 0 {
						t.Fatalf("upstream error not counted: %+v", snap)
					}
				})
			}
		}
	}
}

func TestRelayAuthAndDecodedSizeLimit(t *testing.T) {
	var calls atomic.Int32
	h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }, func(c *Config) { c.MaxBodyBytes = 100 })
	r, err := http.NewRequest(http.MethodPost, s.URL+"/v1/responses", strings.NewReader("invalid gzip"))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Encoding", "gzip")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	res = call(t, s, zipped(t, `{"input":"`+strings.Repeat("x", 1000)+`"}`), true)
	if res.StatusCode != 413 {
		t.Fatal(res.StatusCode)
	}
	res = call(t, s, []byte("invalid gzip"), true)
	if res.StatusCode != 400 {
		t.Fatal(res.StatusCode)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
	waitCompleted(t, h, 2)
	if h.Timings.Snapshot().Errors != 2 {
		t.Fatal("failed requests missing from timings")
	}
}

func TestRelayFlushAndClientCancellation(t *testing.T) {
	canceled := make(chan struct{})
	h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(3 * time.Second):
			t.Error("upstream not canceled")
		}
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer relay-fixture")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "data: first\n\n" {
		t.Fatal(string(b))
	}
	cancel()
	_ = res.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
	waitCompleted(t, h, 1)
	if h.Timings.Snapshot().Errors != 1 {
		t.Fatal("cancellation missing from timing log")
	}
}

func TestRelayUpstreamErrorsAndTimeout(t *testing.T) {
	t.Run("preserve status and body without retry", func(t *testing.T) {
		var calls atomic.Int32
		_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"provider limit"}`)
		}, nil)
		res := call(t, s, []byte(`{}`), false)
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 429 || res.Header.Get("Retry-After") != "7" || string(body) != `{"error":"provider limit"}` || calls.Load() != 1 {
			t.Fatal("upstream error altered or retried")
		}
	})
	t.Run("response headers timeout", func(t *testing.T) {
		_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
				t.Error("upstream timeout did not cancel request")
			}
		}, func(c *Config) { c.ResponseHeaderTimeout = 30 * time.Millisecond })
		res := call(t, s, []byte(`{}`), false)
		if res.StatusCode != 504 {
			t.Fatal(res.StatusCode)
		}
	})
}

func TestRelayUploadTimeout(t *testing.T) {
	var calls atomic.Int32
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, func(c *Config) { c.UploadTimeout = 30 * time.Millisecond })
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(conn, "POST /v1/responses HTTP/1.1\r\nHost: relay\r\nAuthorization: Bearer relay-fixture\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 408 || calls.Load() != 0 {
		t.Fatalf("upload timeout: status=%d upstream calls=%d", res.StatusCode, calls.Load())
	}
}

func TestRelayStatsRequireKey(t *testing.T) {
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }, nil)
	res, err := http.Get(s.URL + "/relay/stats")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	r, err := http.NewRequest(http.MethodGet, s.URL+"/relay/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer relay-fixture")
	res, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var snap TimingSnapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Completed != 0 || snap.Active != 0 {
		t.Fatal(snap)
	}
}

func TestRelayMultipleFixedKeys(t *testing.T) {
	var calls atomic.Int32
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-fixture" {
			t.Error("relay key leaked to upstream")
		}
		w.WriteHeader(http.StatusOK)
	}, func(c *Config) { c.AdditionalKeys = []string{"ecos-fixture", "third-fixture"} })
	for _, test := range []struct {
		auth string
		want int
	}{
		{"Bearer relay-fixture", 200},
		{"Bearer ecos-fixture", 200},
		{"Bearer third-fixture", 200},
		{"Bearer unknown", 401},
		{"ecos-fixture", 401},
		{"", 401},
	} {
		r, err := http.NewRequest(http.MethodGet, s.URL+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", test.auth)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != test.want {
			t.Errorf("auth status = %d, want %d", res.StatusCode, test.want)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream received %d requests, want 3", calls.Load())
	}
}

func waitCompleted(t *testing.T, h *Handler, count uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Timings.Snapshot().Completed >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timing record not completed")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type delayedReader struct {
	io.Reader
	delay time.Duration
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if r.delay != 0 {
		time.Sleep(r.delay)
		r.delay = 0
	}
	return r.Reader.Read(p)
}

func TestRelayFirstTokenBoundaryAndLiveWindow(t *testing.T) {
	headersSeen := make(chan struct{})
	releaseToken := make(chan struct{})
	releaseEnd := make(chan struct{})
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		_ = http.NewResponseController(w).Flush()
		close(headersSeen)
		select {
		case <-releaseToken:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-releaseEnd:
		case <-r.Context().Done():
		}
	}, nil)
	unblockToken := sync.OnceFunc(func() { close(releaseToken) })
	unblockEnd := sync.OnceFunc(func() { close(releaseEnd) })
	t.Cleanup(unblockToken)
	t.Cleanup(unblockEnd)
	transport := h.proxy.Transport
	captured := make(chan *traceState, 1)
	h.proxy.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		state, ok := r.Context().Value(traceKey{}).(*traceState)
		if !ok {
			t.Error("upstream request is missing relay trace state")
			return transport.RoundTrip(r)
		}
		captured <- state
		// This is after HTTP construction, so this delay belongs to stage 3.
		time.Sleep(30 * time.Millisecond)
		return transport.RoundTrip(r)
	})
	request := httptest.NewRequest(http.MethodPost, "http://relay/v1/responses", &delayedReader{Reader: bytes.NewReader(zipped(t, `{"input":"hello"}`)), delay: 30 * time.Millisecond})
	request.Header.Set("Authorization", "Bearer relay-fixture")
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(response, request); close(done) }()
	select {
	case <-headersSeen:
	case <-time.After(time.Second):
		t.Fatal("no upstream headers")
	}
	state := <-captured
	if s := h.Timings.Snapshot(); s.Samples != 0 || s.Active != 1 {
		t.Fatal("response headers counted as first token")
	}
	state.mu.Lock()
	if state.upload < 25*time.Millisecond || state.preparation != state.upstreamStart.Sub(state.uploadDone) {
		t.Error("upload and preparation boundaries overlap or leave a gap")
	}
	state.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	unblockToken()
	deadline := time.Now().Add(time.Second)
	for h.Timings.Snapshot().Samples == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	snap := h.Timings.Snapshot()
	if snap.Samples != 1 || snap.Completed != 0 || snap.Active != 1 || snap.FirstToken.AverageMS == nil || *snap.FirstToken.AverageMS < 55 {
		t.Fatalf("first token excludes connection/upload or waits for response completion: %+v", snap)
	}
	select {
	case <-done:
		t.Error("response completed before stream end")
	default:
	}
	// Cancel through the request context so the test does not wait for cleanup.
	unblockEnd()
	<-done
}

func TestRelayModelsAndNonStreamingResponsesDoNotAddTokenSamples(t *testing.T) {
	h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":"hello"}`)
	}, nil)
	res := call(t, s, []byte(`{}`), false)
	_, _ = io.Copy(io.Discard, res.Body)
	waitCompleted(t, h, 1)
	r, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/models", nil)
	r.Header.Set("Authorization", "Bearer relay-fixture")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	snap := h.Timings.Snapshot()
	if snap.Samples != 0 || snap.Completed != 1 || snap.FirstToken.AverageMS != nil {
		t.Fatal("models/non-streaming response polluted token averages")
	}
}
