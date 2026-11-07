package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/volcengine"
)

type Handler struct {
	config  Config
	keyHash [32]byte
	proxy   *httputil.ReverseProxy
	logger  *slog.Logger
	Timings Timings
}

type traceState struct {
	mu                                                                               sync.Mutex
	start, connectionStart, gotConnection, headersWritten, requestWritten, firstByte time.Time
	upstreamID                                                                       string
	errorKind                                                                        string
}
type traceKey struct{}

func NewHandler(config Config, logger *slog.Logger) (*Handler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	target, err := url.Parse(config.UpstreamURL)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{config: config, keyHash: sha256.Sum256([]byte(config.Key)), logger: logger}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 4096, MaxIdleConnsPerHost: 2048,
		IdleConnTimeout: 2 * time.Minute, TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout, ExpectContinueTimeout: time.Second,
		DisableCompression: true}
	h.proxy = &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1, BufferPool: &copyBuffers{},
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.Header.Set("Authorization", "Bearer "+config.UpstreamKey)
			for _, name := range []string{"X-Api-Key", "X-Goog-Api-Key", "Cookie", "Expect"} {
				p.Out.Header.Del(name)
			}
			p.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(r *http.Response) error {
			if state, ok := r.Request.Context().Value(traceKey{}).(*traceState); ok {
				state.mu.Lock()
				state.upstreamID = boundedID(r.Header.Get("X-Request-Id"))
				state.mu.Unlock()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status, kind := http.StatusBadGateway, "upstream_connection_error"
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				status, kind = http.StatusGatewayTimeout, "upstream_timeout"
			}
			if errors.Is(err, context.Canceled) {
				kind = "request_canceled"
			}
			if state, ok := r.Context().Value(traceKey{}).(*traceState); ok {
				state.mu.Lock()
				state.errorKind = kind
				state.mu.Unlock()
			}
			writeError(w, status, kind)
		},
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": "relay"})
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(digest[:], h.keyHash[:]) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeError(w, http.StatusUnauthorized, "invalid_relay_key")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/relay/stats" {
		writeJSON(w, http.StatusOK, h.Timings.Snapshot())
		return
	}
	path, protocol, ok := route(r.Method, r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "unsupported_relay_endpoint")
		return
	}
	start := time.Now()
	state := &traceState{start: start}
	record := Timing{At: start.UTC(), RequestID: boundedID(r.Header.Get("X-Request-Id")), Path: r.URL.Path}
	if record.RequestID == "" {
		var id [12]byte
		if _, err := rand.Read(id[:]); err == nil {
			record.RequestID = hex.EncodeToString(id[:])
		}
	}
	w.Header().Set("X-Relay-Request-Id", record.RequestID)
	tracked := &responseWriter{ResponseWriter: w}
	uploadDone := start
	h.Timings.begin()
	defer func() {
		panicked := recover()
		record.Status = tracked.status
		if record.Status == 0 {
			record.Status = http.StatusOK
		}
		record.ResponseBytes = tracked.bytes
		record.TotalMS = milliseconds(time.Since(start))
		record.ProcessingMS = milliseconds(time.Since(uploadDone))
		state.mu.Lock()
		record.UpstreamRequestID, record.Error = state.upstreamID, state.errorKind
		if !state.gotConnection.IsZero() {
			record.UpstreamConnectMS = durationPointer(state.gotConnection.Sub(state.connectionStart))
		}
		if !state.requestWritten.IsZero() && !state.headersWritten.IsZero() {
			record.UpstreamWriteMS = durationPointer(state.requestWritten.Sub(state.headersWritten))
		}
		if !state.firstByte.IsZero() {
			record.UpstreamFirstByteMS = durationPointer(state.firstByte.Sub(state.start))
		}
		state.mu.Unlock()
		if tracked.writeError {
			record.Error = "downstream_write_error"
		}
		if r.Context().Err() != nil {
			record.Error = "request_canceled"
		}
		if panicked != nil && record.Error == "" {
			record.Error = "stream_interrupted"
		}
		h.Timings.finish(record)
		h.logger.Info("relay_request", "timing", record)
		if panicked != nil {
			panic(panicked)
		}
	}()
	if r.Method == http.MethodPost {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(start.Add(h.config.UploadTimeout))
		body, wasGzip, err := readBody(tracked, r, h.config.MaxBodyBytes)
		_ = controller.SetReadDeadline(time.Time{})
		uploadDone = time.Now()
		record.UploadMS = milliseconds(uploadDone.Sub(start))
		record.Gzip = wasGzip
		if err != nil {
			status, kind := http.StatusBadRequest, "invalid_request_body"
			var tooLarge *http.MaxBytesError
			var timeout net.Error
			if errors.As(err, &tooLarge) {
				status, kind = http.StatusRequestEntityTooLarge, "request_body_too_large"
			}
			if errors.As(err, &timeout) && timeout.Timeout() {
				status, kind = http.StatusRequestTimeout, "request_upload_timeout"
			}
			state.errorKind = kind
			writeError(tracked, status, kind)
			return
		}
		record.BodyBytes = int64(len(body))
		if h.config.Compatibility {
			body, err = volcengine.NormalizeRequest(body, protocol)
			if err != nil {
				state.errorKind = "invalid_json"
				writeError(tracked, http.StatusBadRequest, "invalid_json")
				return
			}
		}
		record.CompatibilityMS = milliseconds(time.Since(uploadDone))
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = nil
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
	}
	trace := &httptrace.ClientTrace{
		GetConn:      func(string) { state.mu.Lock(); state.connectionStart = time.Now(); state.mu.Unlock() },
		GotConn:      func(httptrace.GotConnInfo) { state.mu.Lock(); state.gotConnection = time.Now(); state.mu.Unlock() },
		WroteHeaders: func() { state.mu.Lock(); state.headersWritten = time.Now(); state.mu.Unlock() },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			state.mu.Lock()
			if info.Err == nil {
				state.requestWritten = time.Now()
			}
			state.mu.Unlock()
		},
		GotFirstResponseByte: func() { state.mu.Lock(); state.firstByte = time.Now(); state.mu.Unlock() },
	}
	ctx := httptrace.WithClientTrace(context.WithValue(r.Context(), traceKey{}, state), trace)
	r = r.WithContext(ctx)
	r.URL.Path = path
	r.URL.RawPath = ""
	state.start = time.Now()
	h.proxy.ServeHTTP(tracked, r)
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool, error) {
	defer func() { _ = r.Body.Close() }()
	var reader io.Reader = http.MaxBytesReader(w, r.Body, limit)
	compressed := strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip")
	if compressed {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, true, err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	} else if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, false, errors.New("unsupported content encoding")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if int64(len(body)) > limit {
		return nil, compressed, &http.MaxBytesError{Limit: limit}
	}
	return body, compressed, err
}

func route(method, path string) (string, string, bool) {
	if method == http.MethodGet && (path == "/models" || path == "/v1/models") {
		return "/models", "", true
	}
	if method != http.MethodPost {
		return "", "", false
	}
	switch path {
	case "/responses", "/v1/responses":
		return "/responses", volcengine.ProtocolResponses, true
	case "/chat/completions", "/v1/chat/completions":
		return "/chat/completions", volcengine.ProtocolChatCompletions, true
	default:
		return "", "", false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, kind string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": "relay_error", "message": kind}})
}
func boundedID(s string) string {
	if len(s) > 128 {
		return s[:128]
	}
	return s
}
func milliseconds(d time.Duration) float64     { return float64(d) / float64(time.Millisecond) }
func durationPointer(d time.Duration) *float64 { v := milliseconds(d); return &v }

type responseWriter struct {
	http.ResponseWriter
	status     int
	bytes      int64
	writeError bool
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	if err != nil {
		w.writeError = true
	}
	return n, err
}

type copyBuffers struct{ pool sync.Pool }

func (p *copyBuffers) Get() []byte {
	if b, ok := p.pool.Get().(*[32 << 10]byte); ok {
		return b[:]
	}
	return make([]byte, 32<<10)
}
func (p *copyBuffers) Put(b []byte) {
	if len(b) == 32<<10 {
		p.pool.Put((*[32 << 10]byte)(b))
	}
}
