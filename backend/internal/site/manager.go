package site

import (
	"bytes"
	"compress/gzip"
	"context"
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
	cfg              Config
	store            Store
	client           *http.Client
	proxy            *httputil.ReverseProxy
	maxBody          int64
	BodyLimit        func() int64
	current          atomic.Pointer[lease]
	identityVerified atomic.Bool
	lastSync         atomic.Pointer[SyncStats]
	peers            map[string]*peerSnapshot
	version          string // owned by the single synchronization loop
	primaryID        string
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	deliveryGate     sync.Mutex
	now              func() time.Time
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
	m.client = &http.Client{Transport: transport, Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		m.run(ctx)
	}()
	go func() { defer m.wg.Done(); m.runDelivery(ctx) }()
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
	m.deliveryGate.Lock()
	defer m.deliveryGate.Unlock()
	wallStarted, started := time.Now(), m.now()
	// Initial paged bootstrap may take longer than a lease. It stays closed until
	// an additional fresh delta succeeds; page fetches never extend validity.
	timeout := 12 * time.Second
	if m.version == "" {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	covered := true
	if store, ok := m.store.(interface {
		PendingCoverage(context.Context) (bool, error)
	}); ok {
		var err error
		covered, err = store.PendingCoverage(ctx)
		if err != nil {
			return err
		}
	}
	ids, err := m.store.PendingIDs(ctx)
	if err != nil {
		return err
	}
	input := SyncRequest{Protocol: ProtocolVersion, NodeID: m.cfg.ID, Version: m.version, Schema: m.store.Schema(), PendingIDs: ids}
	if _, modern := m.store.(DeliveryStore); !modern {
		input.Events, err = m.store.Pending(ctx, 128)
		if err != nil {
			return err
		}
	}
	var output SyncResponse
	var stats SyncStats
	for {
		var page SyncResponse
		transfer, e := m.postSite(ctx, SyncPath, &input, &page)
		if e != nil {
			return e
		}
		if page.Protocol != ProtocolVersion || page.NodeID != m.cfg.ID || page.Schema != m.store.Schema() || page.Version == "" || page.PrimaryID == "" || (!page.Full && page.BaseVersion != m.version) {
			return errors.New("invalid site synchronization response")
		}
		if m.primaryID != "" && page.PrimaryID != m.primaryID {
			return errors.New("primary identity changed")
		}
		if input.Cursor == "" {
			output = page
		} else {
			if page.Version != output.Version || page.Full != output.Full || page.PrimaryID != output.PrimaryID {
				return errors.New("snapshot changed during paging")
			}
			output.Rows = append(output.Rows, page.Rows...)
		}
		stats.UploadJSONBytes += transfer.uploadJSON
		stats.UploadWireBytes += transfer.uploadWire
		stats.DownloadJSONBytes += transfer.downloadJSON
		stats.DownloadWireBytes += transfer.downloadWire
		if page.Cursor == "" {
			break
		}
		if page.Cursor == input.Cursor {
			return errors.New("snapshot cursor did not advance")
		}
		input.TargetVersion, input.Cursor = page.Version, page.Cursor
	}
	if err = validateReceipts(ids, output.Acknowledged, output.Rejected, false); err != nil {
		return err
	}
	for _, e := range input.Events {
		found := false
		for _, id := range output.Acknowledged {
			found = found || id == e.ID
		}
		if !found {
			return errors.New("incomplete event acknowledgement")
		}
	}
	// Small/ordinary refreshes retain the strict request-start lease bound.
	_, pagedStore := m.store.(DeliveryStore)
	if (!output.Full || !pagedStore) && !m.now().Before(started.Add(ReplicaTTL)) {
		return errors.New("synchronization response expired")
	}
	if err = m.store.Install(ctx, output); err != nil {
		m.current.Store(nil)
		m.version = ""
		return err
	}
	m.version, m.primaryID = output.Version, output.PrimaryID
	m.identityVerified.Store(true)
	if !covered {
		m.current.Store(nil)
		return errors.New("legacy billing receipts still reconciling; lease remains closed")
	}
	if !m.now().Before(started.Add(ReplicaTTL)) {
		m.current.Store(nil)
		return errors.New("bootstrap installed; fresh delta required before serving")
	}
	m.current.Store(&lease{expires: started.Add(ReplicaTTL)})
	stats.CompletedAt = time.Now().UTC()
	stats.DurationMS = time.Since(wallStarted).Milliseconds()
	stats.Full = output.Full
	stats.Rows, stats.Events, stats.Pending = len(output.Rows), len(input.Events), len(ids)
	m.lastSync.Store(&stats)
	slog.Info("site synchronization completed", "node", m.cfg.ID, "full", stats.Full, "rows", stats.Rows, "events", stats.Events, "pending", stats.Pending, "duration_ms", stats.DurationMS, "upload_json_bytes", stats.UploadJSONBytes, "upload_wire_bytes", stats.UploadWireBytes, "download_json_bytes", stats.DownloadJSONBytes, "download_wire_bytes", stats.DownloadWireBytes)
	return nil
}

func (m *Manager) SyncHandler(c *gin.Context) {
	peer := m.peer(c)
	if peer == nil {
		return
	}
	var input SyncRequest
	if err := decodeSiteRequest(c, &input); err != nil || input.Protocol != ProtocolVersion || input.NodeID != peer.ID || input.Schema != m.store.Schema() || len(input.Events) > 128 || len(input.PendingIDs) > MaxPendingBills {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if store, ok := m.store.(DeliveryStore); ok {
		if len(input.Events) > 0 {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		timeout := 10 * time.Second
		if input.Version == "" {
			timeout = 90 * time.Second
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		snapshot, err := store.ExportPage(ctx, *peer, input)
		if err != nil {
			slog.Error("site export failed", "node", peer.ID, "error", err)
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		encodeSiteResponse(c, SyncResponse{Protocol: ProtocolVersion, PrimaryID: m.cfg.ID, NodeID: peer.ID, BaseVersion: input.Version, Version: snapshot.Version, Schema: snapshot.Schema, Full: snapshot.Full, Rows: snapshot.Rows, Acknowledged: snapshot.Acknowledged, Rejected: snapshot.Rejected, Cursor: snapshot.Cursor})
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
