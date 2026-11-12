package site

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxSyncBody = 32 << 20

type lease struct {
	expires time.Time // monotonic; never persisted or resurrected after restart
}
type peerSnapshot struct {
	mu       sync.Mutex
	version  string
	snapshot Snapshot
}

type Manager struct {
	cfg       Config
	store     Store
	client    *http.Client
	proxy     *httputil.ReverseProxy
	maxBody   int64
	BodyLimit func() int64
	current   atomic.Pointer[lease]
	lastSync  atomic.Pointer[SyncStats]
	peers     map[string]*peerSnapshot
	version   string // owned by the single synchronization loop
	primaryID string
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	now       func() time.Time
}

func New(cfg Config, store Store, maxBody int64) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.EffectiveMode() != Standalone && store == nil {
		return nil, errors.New("site store is required")
	}
	if maxBody <= 0 {
		maxBody = 256 << 20
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("site requires the standard HTTP transport")
	}
	transport := defaultTransport.Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 128
	transport.ResponseHeaderTimeout = 0 // long thinking/streams use request cancellation
	m := &Manager{cfg: cfg, store: store, maxBody: maxBody, now: time.Now, peers: map[string]*peerSnapshot{}}
	m.client = &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range cfg.Peers {
		m.peers[p.ID] = &peerSnapshot{}
	}
	if cfg.EffectiveMode() == Secondary {
		target, _ := url.Parse(cfg.PrimaryURL)
		m.proxy = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(target)
				r.Out.Host = target.Host
				r.SetXForwarded()
				if id, ok := r.In.Context().Value(ctxkey.RequestID).(string); ok && id != "" {
					r.Out.Header.Set("X-Request-ID", id)
				}
			},
			Transport: transport, FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				slog.Warn("site primary forwarding failed", "error", err)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"type":"primary_unavailable","message":"Primary site unavailable"}}`)
			},
		}
	}
	return m, nil
}

func (m *Manager) Start() {
	if m.cfg.EffectiveMode() != Secondary {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(SyncInterval)
		defer ticker.Stop()
		for {
			if err := m.syncOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("site synchronization failed; lease was not renewed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	m.current.Store(nil)
	if m.client != nil {
		m.client.CloseIdleConnections()
	}
}

func (m *Manager) Ready() bool {
	l := m.current.Load()
	return l != nil && m.now().Before(l.expires)
}

func (m *Manager) syncOnce(parent context.Context) error {
	// Bound validity by the START of the round trip. A delayed response must
	// never give an already stale database snapshot another fifteen seconds.
	wallStarted := time.Now()
	started := m.now()
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	events, err := m.store.Pending(ctx, 128)
	if err != nil {
		return err
	}
	ids, err := m.store.PendingIDs(ctx)
	if err != nil {
		return err
	}
	input := SyncRequest{Protocol: ProtocolVersion, NodeID: m.cfg.ID, Version: m.version, Schema: m.store.Schema(), Events: events, PendingIDs: ids}
	raw, err := marshalSyncRequest(&input, maxSyncBody)
	if err != nil {
		return err
	}
	events = input.Events
	packed, err := compress(raw)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.cfg.PrimaryURL, "/")+SyncPath, bytes.NewReader(packed))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	// Decode explicitly so byte accounting measures the compressed response,
	// independent of net/http's transparent gzip behavior.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("primary sync status %d", resp.StatusCode)
	}
	var output SyncResponse
	wire := &countingReader{r: io.LimitReader(resp.Body, maxSyncBody+1)}
	var reader io.Reader = wire
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, e := gzip.NewReader(wire)
		if e != nil {
			return e
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	} else if resp.Header.Get("Content-Encoding") != "" {
		return errors.New("unsupported synchronization encoding")
	}
	decoded := &countingReader{r: reader}
	if err = decodeLimited(decoded, &output, maxSyncBody); err != nil {
		return err
	}
	if output.Protocol != ProtocolVersion || output.NodeID != m.cfg.ID || output.Schema != m.store.Schema() || output.Version == "" || output.PrimaryID == "" || (!output.Full && output.BaseVersion != m.version) {
		return errors.New("invalid site synchronization response")
	}
	if m.primaryID != "" && output.PrimaryID != m.primaryID {
		return errors.New("primary identity changed")
	}
	sent := make(map[int64]bool, len(events))
	for _, id := range ids {
		sent[id] = true
	}
	seen := map[int64]bool{}
	for _, id := range output.Acknowledged {
		if !sent[id] || seen[id] {
			return errors.New("invalid event acknowledgement")
		}
		seen[id] = true
	}
	for _, e := range events {
		if !seen[e.ID] {
			return errors.New("incomplete event acknowledgement")
		}
	}
	if !m.now().Before(started.Add(ReplicaTTL)) {
		return errors.New("synchronization response expired")
	}
	if err = m.store.Install(ctx, output); err != nil {
		// SQL may have committed before a cache/pricing reload failed. Keep the
		// node closed until one complete installation succeeds. Force a full
		// snapshot next time because the DB may now be ahead of our diff cursor.
		m.current.Store(nil)
		m.version = ""
		return err
	}
	m.version = output.Version
	m.primaryID = output.PrimaryID
	if !m.now().Before(started.Add(ReplicaTTL)) {
		m.current.Store(nil)
		return errors.New("replica installation expired")
	}
	m.current.Store(&lease{expires: started.Add(ReplicaTTL)})
	stats := SyncStats{CompletedAt: time.Now().UTC(), DurationMS: time.Since(wallStarted).Milliseconds(), Full: output.Full,
		Rows: len(output.Rows), Events: len(events), Pending: len(ids), UploadJSONBytes: len(raw), UploadWireBytes: len(packed),
		DownloadJSONBytes: decoded.n, DownloadWireBytes: wire.n}
	m.lastSync.Store(&stats)
	slog.Info("site synchronization completed", "node", m.cfg.ID, "full", stats.Full, "rows", stats.Rows,
		"events", stats.Events, "pending", stats.Pending, "duration_ms", stats.DurationMS,
		"upload_json_bytes", stats.UploadJSONBytes, "upload_wire_bytes", stats.UploadWireBytes,
		"download_json_bytes", stats.DownloadJSONBytes, "download_wire_bytes", stats.DownloadWireBytes)
	return nil
}

func (m *Manager) SyncHandler(c *gin.Context) {
	if m.cfg.EffectiveMode() != Primary {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	var peer *Peer
	for i := range m.cfg.Peers {
		p := &m.cfg.Peers[i]
		if subtle.ConstantTimeCompare([]byte(p.Token), []byte(token)) == 1 {
			peer = p
			break
		}
	}
	if peer == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	var reader io.Reader = http.MaxBytesReader(c.Writer, c.Request.Body, maxSyncBody)
	if c.GetHeader("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	} else if c.GetHeader("Content-Encoding") != "" {
		c.AbortWithStatus(http.StatusUnsupportedMediaType)
		return
	}
	var input SyncRequest
	if err := decodeLimited(reader, &input, maxSyncBody); err != nil || input.Protocol != ProtocolVersion || input.NodeID != peer.ID || input.Schema != m.store.Schema() || len(input.Events) > 128 || len(input.PendingIDs) > 1000000 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	state := m.peers[peer.ID]
	state.mu.Lock()
	defer state.mu.Unlock()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	snapshot, err := m.store.Export(ctx, *peer, input.Events, input.PendingIDs)
	if err != nil {
		slog.Error("site export failed", "node", peer.ID, "error", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site synchronization unavailable"})
		return
	}
	version := snapshotVersion(snapshot)
	output := SyncResponse{Protocol: ProtocolVersion, PrimaryID: m.cfg.ID, NodeID: peer.ID, BaseVersion: input.Version, Version: version, Schema: snapshot.Schema}
	if state.version != "" && state.version == input.Version {
		output.Rows = diffRows(state.snapshot.Rows, snapshot.Rows)
	} else {
		output.Full = true
		output.Rows = snapshot.Rows
	}
	output.Acknowledged = snapshot.Acknowledged
	state.version = version
	state.snapshot = snapshot
	raw, err := json.Marshal(output)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if len(raw) > maxSyncBody {
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}
	packed, err := compress(raw)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Encoding", "gzip")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", packed)
}

func (m *Manager) Ingress() gin.HandlerFunc {
	return func(c *gin.Context) {
		if m.cfg.EffectiveMode() != Secondary {
			c.Next()
			return
		}
		if c.Request.URL.Path == "/health" || c.Request.URL.Path == "/healthz" || c.Request.URL.Path == "/readyz" {
			c.Next()
			return
		}
		if !modelPath(c.Request.URL.Path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if !m.Ready() {
			unavailable(c)
			return
		}
		// WebSocket messages cannot be classified by an HTTP body. Keep the
		// original upgrade and route the session to the primary.
		if strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
			m.proxy.ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}
		bodyLimit := m.maxBody
		if m.BodyLimit != nil {
			if n := m.BodyLimit(); n > 0 {
				bodyLimit = n
			}
		}
		local := false
		if c.Request.Method == http.MethodPost && strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "application/json") {
			wire, err := io.ReadAll(io.LimitReader(c.Request.Body, bodyLimit+1))
			_ = c.Request.Body.Close()
			if err != nil {
				abortBodyRead(c, err)
				return
			}
			if int64(len(wire)) > bodyLimit {
				c.AbortWithStatus(http.StatusRequestEntityTooLarge)
				return
			}
			body := wire
			if c.GetHeader("Content-Encoding") != "" {
				inspect := c.Request.Clone(c.Request.Context())
				inspect.Body = io.NopCloser(bytes.NewReader(wire))
				body, err = pkghttputil.ReadRequestBodyWithPreallocLimit(inspect, bodyLimit)
				if err != nil {
					abortBodyRead(c, err)
					return
				}
			}
			model := gjson.GetBytes(body, "model").String()
			if len(model) <= 256 {
				c.Set("ops_model", model)
			}
			c.Set("ops_stream", gjson.GetBytes(body, "stream").Bool())
			local = m.store.MatchLocalModel(requestKey(c.Request), model, modelEndpoint(c.Request.URL.Path))

			if local {
				// Restore the original wire body; the gateway owns decoding and
				// all protocol-specific validation and compatibility transformations.
				c.Request.Body = io.NopCloser(bytes.NewReader(wire))
			} else {
				if c.GetHeader("Content-Encoding") == "" && len(wire) >= 1024 {
					packed, err := compress(wire)
					if err != nil {
						c.AbortWithStatus(http.StatusInternalServerError)
						return
					}
					if len(packed) < len(wire) {
						wire = packed
						c.Request.Header.Set("Content-Encoding", "gzip")
					}
				}
				c.Request.Body = io.NopCloser(bytes.NewReader(wire))
				c.Request.ContentLength = int64(len(wire))
				c.Request.Header.Del("Content-Length")
			}
		}
		if !m.Ready() {
			unavailable(c)
			return
		}
		if local {
			c.Next()
			return
		}
		m.proxy.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}
}

func abortBodyRead(c *gin.Context, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}
	c.AbortWithStatus(http.StatusBadRequest)
}

func unavailable(c *gin.Context) {
	c.Header("Retry-After", "5")
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "replica_expired", "message": "Primary synchronization is unavailable; retry after synchronization recovers"}})
}

func modelPath(p string) bool {
	return strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/v1beta/") || p == "/responses" || strings.HasPrefix(p, "/responses/") || p == "/chat/completions" || p == "/messages" || p == "/messages/count_tokens" || p == "/models" || strings.HasPrefix(p, "/models/") || strings.HasPrefix(p, "/backend-api/codex/") || strings.HasPrefix(p, "/antigravity/") || p == "/alpha/search"
}

func compress(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err = w.Write(raw); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Large diagnostics must not make a whole outbox page permanently exceed the
// control-plane limit. Preserve all pending IDs for acknowledgement/overlay
// reconciliation, but send only the prefix of events that fits this round.
func marshalSyncRequest(input *SyncRequest, maxBytes int) ([]byte, error) {
	events := input.Events
	input.Events = nil
	metadata, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(metadata) > maxBytes {
		return nil, errors.New("site synchronization metadata exceeds body limit")
	}
	size := len(metadata) + len(`,"events":[]`)
	count := 0
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		size += len(encoded)
		if count > 0 {
			size++ // array separator
		}
		if size > maxBytes {
			break
		}
		count++
	}
	if len(events) > 0 && count == 0 {
		return nil, fmt.Errorf("site event %d exceeds synchronization body limit", events[0].ID)
	}
	input.Events = events[:count]
	return json.Marshal(input)
}

func decodeLimited(r io.Reader, target any, max int64) error {
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > max {
		return errors.New("site message too large")
	}
	return json.Unmarshal(raw, target)
}

// Endpoint names match the existing composite route catalog.
func modelEndpoint(p string) string {
	switch {
	case strings.HasSuffix(p, "/messages/count_tokens"):
		return "count_tokens"
	case strings.HasSuffix(p, "/messages"):
		return "messages"
	case strings.HasSuffix(p, "/chat/completions"):
		return "chat_completions"
	case strings.HasSuffix(p, "/embeddings"):
		return "embeddings"
	case strings.Contains(p, "/images/"):
		return "images"
	default:
		return "responses"
	}
}

// This is only a replica-index lookup. The selected gateway still performs all
// authentication, and the proxy always forwards the untouched client headers.
func requestKey(r *http.Request) string {
	if parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		if key := strings.TrimSpace(parts[1]); key != "" {
			return key
		}
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	return r.Header.Get("x-goog-api-key")
}
