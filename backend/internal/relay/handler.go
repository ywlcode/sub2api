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
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Handler struct {
	config    Config
	keyHashes [][32]byte
	proxy     *httputil.ReverseProxy
	logger    *slog.Logger
	Timings   Timings
}

type traceState struct {
	mu            sync.Mutex
	uploadDone    time.Time
	upstreamStart time.Time
	upload        time.Duration
	preparation   time.Duration
	firstToken    bool
	errorKind     string
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
	h := &Handler{config: config, keyHashes: [][32]byte{sha256.Sum256([]byte(config.Key))}, logger: logger}
	for _, key := range config.AdditionalKeys {
		h.keyHashes = append(h.keyHashes, sha256.Sum256([]byte(key)))
	}
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
			if state, ok := p.Out.Context().Value(traceKey{}).(*traceState); ok {
				state.mu.Lock()
				state.upstreamStart = time.Now()
				state.preparation = state.upstreamStart.Sub(state.uploadDone)
				state.mu.Unlock()
			}
		},
		ModifyResponse: func(r *http.Response) error {
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			encoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
			if state, ok := r.Request.Context().Value(traceKey{}).(*traceState); ok && r.Request.Method == http.MethodPost && r.StatusCode >= 200 && r.StatusCode < 300 && mediaType == "text/event-stream" && (encoding == "" || strings.EqualFold(encoding, "identity")) {
				r.Body = &firstTokenBody{ReadCloser: r.Body, observer: deltaObserver{first: func(at time.Time) {
					state.mu.Lock()
					if state.firstToken || state.upstreamStart.IsZero() {
						state.mu.Unlock()
						return
					}
					state.firstToken = true
					upload, preparation, firstToken := state.upload, state.preparation, at.Sub(state.upstreamStart)
					state.mu.Unlock()
					h.Timings.record(upload, preparation, firstToken)
				}}}
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
	receivedAt := time.Now()
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": "relay"})
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(provided))
	matched := 0
	for _, keyHash := range h.keyHashes {
		matched |= subtle.ConstantTimeCompare(digest[:], keyHash[:])
	}
	if matched != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeError(w, http.StatusUnauthorized, "invalid_relay_key")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/relay/stats" {
		writeJSON(w, http.StatusOK, h.Timings.Snapshot())
		return
	}
	path, ok := route(r.Method, r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "unsupported_relay_endpoint")
		return
	}
	state := &traceState{}
	requestID := boundedID(r.Header.Get("X-Request-Id"))
	if requestID == "" {
		var id [12]byte
		if _, err := rand.Read(id[:]); err == nil {
			requestID = hex.EncodeToString(id[:])
		}
	}
	w.Header().Set("X-Relay-Request-Id", requestID)
	tracked := &responseWriter{ResponseWriter: w}
	countStats := r.Method == http.MethodPost
	if countStats {
		h.Timings.begin()
	}
	defer func() {
		panicked := recover()
		state.mu.Lock()
		failed := state.errorKind != "" || tracked.status >= 400 || tracked.writeError || r.Context().Err() != nil || panicked != nil
		state.mu.Unlock()
		if countStats {
			h.Timings.finish(failed)
		}
		if panicked != nil {
			panic(panicked)
		}
	}()
	if r.Method == http.MethodPost {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(receivedAt.Add(h.config.UploadTimeout))
		// Finish receiving the wire body before decompressing it, so the three
		// timing intervals are contiguous and never overlap.
		wire, err := readWireBody(tracked, r, h.config.MaxBodyBytes)
		_ = controller.SetReadDeadline(time.Time{})
		state.uploadDone = time.Now()
		state.upload = state.uploadDone.Sub(receivedAt)
		var body []byte
		if err == nil {
			body, err = decodeBody(wire, r.Header.Get("Content-Encoding"), h.config.MaxBodyBytes)
		}
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
		// Forward the decoded bytes unchanged. Protocol and provider-specific
		// normalization belongs to the full gateway before it calls the relay.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = nil
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
	}
	ctx := context.WithValue(r.Context(), traceKey{}, state)
	r = r.WithContext(ctx)
	r.URL.Path = path
	r.URL.RawPath = ""
	h.proxy.ServeHTTP(tracked, r)
}

func readWireBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}

func decodeBody(wire []byte, encoding string, limit int64) ([]byte, error) {
	encoding = strings.TrimSpace(encoding)
	if strings.EqualFold(encoding, "gzip") {
		gz, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		body, err := io.ReadAll(io.LimitReader(gz, limit+1))
		if int64(len(body)) > limit {
			return nil, &http.MaxBytesError{Limit: limit}
		}
		return body, err
	}
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, errors.New("unsupported content encoding")
	}
	return wire, nil
}

func route(method, path string) (string, bool) {
	if method == http.MethodGet && (path == "/models" || path == "/v1/models") {
		return "/models", true
	}
	if method != http.MethodPost {
		return "", false
	}
	switch path {
	case "/responses", "/v1/responses":
		return "/responses", true
	case "/chat/completions", "/v1/chat/completions":
		return "/chat/completions", true
	default:
		return "", false
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
func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

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
