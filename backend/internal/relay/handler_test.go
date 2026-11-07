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
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, upstream http.HandlerFunc, change func(*Config)) (*Handler, *httptest.Server) {
	t.Helper()
	u := httptest.NewServer(upstream)
	t.Cleanup(u.Close)
	c := Config{Key: "relay-fixture", UpstreamKey: "upstream-fixture", UpstreamURL: u.URL + "/api/v3", Compatibility: true, MaxBodyBytes: 1 << 20, UploadTimeout: time.Second, ResponseHeaderTimeout: time.Second}
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
	r, err := http.NewRequest(http.MethodPost, s.URL+"/v1/responses", bytes.NewReader(body))
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

func TestRelayGzipCompatibilityAndBytePreservingStream(t *testing.T) {
	body := `{"model":"deepseek","seed":9007199254740993,"reasoning":{"effort":"off"},"input":[{"role":"user","content":"<thinking>literal</thinking>"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"history"}]}]}`
	stream := "event: response.output_text.delta\ndata: {\"delta\":\"<tag>\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	seen := make(chan []byte, 1)
	h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/responses" || r.Header.Get("Authorization") != "Bearer upstream-fixture" || r.Header.Get("Content-Encoding") != "" {
			t.Error("incorrect upstream URL/auth/encoding")
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		seen <- b
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "ark-fixture")
		_, _ = io.WriteString(w, stream)
	}, nil)
	res := call(t, s, zipped(t, body), true)
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || string(out) != stream {
		t.Fatalf("response changed: %d %q", res.StatusCode, out)
	}
	wire := <-seen
	for _, want := range []string{`"seed":9007199254740993`, `"effort":"none"`, `<thinking>literal</thinking>`, `"type":"message"`, `"summary":[{"text":"history","type":"summary_text"}]`} {
		if !bytes.Contains(wire, []byte(want)) {
			t.Errorf("missing compatibility result %s in %s", want, wire)
		}
	}
	waitCompleted(t, h, 1)
	snap := h.Timings.Snapshot()
	if snap.Errors != 0 || len(snap.Recent) != 1 || !snap.Recent[0].Gzip || snap.Recent[0].BodyBytes != int64(len(body)) || snap.Recent[0].UpstreamRequestID != "ark-fixture" || snap.Recent[0].UpstreamWriteMS == nil {
		t.Fatalf("bad timings: %+v", snap)
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
	res = call(t, s, []byte(`{} {}`), false)
	if res.StatusCode != 400 {
		t.Fatal(res.StatusCode)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
	waitCompleted(t, h, 3)
	if h.Timings.Snapshot().Errors != 3 {
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
