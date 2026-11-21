package site

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// A capable secondary sends "-" (cold) or its single known manifest SHA.
// Only the final configuration page can confirm a root. "=" confirms exactly
// the digest in that request. Absence, including on an old primary, revokes.
// Headers leave even an exactly 32 MiB configuration page its entire budget.
const (
	catalogHeader       = "X-Site-Catalog-V1"
	catalogSizeHeader   = "X-Site-Catalog-Size"
	catalogQuery        = "catalog=v1"
	catalogRefresh      = 30 * time.Second
	catalogMinRefresh   = 15 * time.Second
	catalogMaxAge       = 60 * time.Second
	catalogBuildTimeout = 12 * time.Second
	catalogGateTimeout  = 50 * time.Millisecond
)

// One grant per configured peer, protected by catalogControl.mu. Replica IDs
// and config versions are peer-specific, even when the rendered bytes are shared.
type catalogGrant struct {
	replica, schema, version string
	minGeneration            uint64
	check                    uint64
	root                     string
}

type catalogControl struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	working bool // one builder OR one download batch per manager
	gates   chan struct{}
	pulls   chan struct{}

	// Primary only. The image is immutable and shared by all eligible peers.
	image                 *catalogImage
	started, published    uint64
	lastStart, validUntil time.Time
}

func (c *catalogControl) init() {
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.gates = make(chan struct{}, 4)
	c.pulls = make(chan struct{}, 4)
}

func (c *catalogControl) startLocked(work func()) {
	c.working = true
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			c.working = false
			c.mu.Unlock()
		}()
		work()
	}()
}

func (c *catalogControl) stop() {
	c.mu.Lock()
	c.cancel()
	c.image = nil
	c.mu.Unlock()
	c.wg.Wait()
}

// An unhealthy cache hook may veto discovery, but must never hold a lease
// request past its small deadline or start an unbounded number of goroutines.
// Each invocation owns its result; a late true is discarded.
func (m *Manager) allowCatalog(ctx context.Context) bool {
	check := m.AllowDiscovery
	if check == nil {
		return true
	}
	select {
	case m.catalog.gates <- struct{}{}:
	default:
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, catalogGateTimeout)
	defer cancel()
	result := make(chan bool, 1)
	go func() {
		defer func() { <-m.catalog.gates }()
		result <- check(ctx)
	}()
	select {
	case <-ctx.Done():
		return false
	case ok := <-result:
		return ok && ctx.Err() == nil
	}
}

// These changes explicitly invalidate the catalog's group scope or rendering.
// Ordinary debit rows can reuse a root: the receiver still requires a new
// configuration-bound view and checks the primary's per-group policy bytes.
func catalogScopeChanged(page *SyncResponse) bool {
	if page.Full || page.Cursor != "" {
		return true
	}
	for _, r := range page.Rows {
		switch r.Table {
		case "groups", "account_groups", "api_key_groups", "composite_model_routes", "settings":
			return true
		case "accounts", "api_keys":
			if len(r.Value) == 0 {
				return true
			}
		}
	}
	return false
}

func (m *Manager) advertiseCatalog(c *gin.Context, peer *Peer, req *SyncRequest, page *SyncResponse) {
	known := c.GetHeader(catalogHeader)
	if m.LoadCatalog == nil || known != "-" && !validCatalogDigest(known) {
		return
	}
	source := &m.catalog
	source.mu.Lock()
	grant := &m.peers[peer.ID].catalog
	if grant.version != page.Version || grant.replica != req.ReplicaID || grant.schema != req.Schema {
		if grant.version == "" || grant.version != req.Version || grant.replica != req.ReplicaID || grant.schema != req.Schema || req.Cursor != "" || catalogScopeChanged(page) {
			// A build already in progress may have read the old allowlist.
			grant.minGeneration = source.started + 1
			grant.root = ""
		}
		grant.version, grant.replica, grant.schema = page.Version, req.ReplicaID, req.Schema
	}
	grant.check++
	check := grant.check
	if page.Cursor != "" {
		grant.root = ""
		source.mu.Unlock()
		return
	}
	source.mu.Unlock()
	// Check the live veto after mandatory export, including on unchanged
	// heartbeats. A later check or withdrawal supersedes this invocation.
	allowed := m.allowCatalog(c.Request.Context())
	source.mu.Lock()
	defer source.mu.Unlock()
	if check != grant.check {
		return
	}
	grant.root = ""
	if !allowed || source.ctx.Err() != nil {
		return
	}
	now := m.now()
	interval := catalogRefresh
	if grant.minGeneration > source.published {
		interval = catalogMinRefresh
	}
	if !source.working && (source.lastStart.IsZero() || !now.Before(source.lastStart.Add(interval))) {
		source.lastStart = now
		source.started++
		source.startLocked(func() {
			ctx, cancel := context.WithTimeout(source.ctx, catalogBuildTimeout)
			defer cancel()
			groups, err := m.LoadCatalog(ctx)
			var image *catalogImage
			if err == nil {
				image, err = buildCatalog(ctx, groups)
			}
			if err == nil {
				err = ctx.Err()
			}
			source.mu.Lock()
			defer source.mu.Unlock()
			if err != nil || source.ctx.Err() != nil {
				source.image = nil
				if source.ctx.Err() == nil {
					slog.Warn("site catalog refresh failed", "error", err)
				}
				return
			}
			source.image, source.published, source.validUntil = image, source.started, now.Add(catalogMaxAge)
		})
	}
	if source.image == nil || source.published < grant.minGeneration || !now.Before(source.validUntil) {
		return
	}
	grant.root = source.image.root
	if grant.root == known {
		c.Header(catalogHeader, "=")
	} else {
		c.Header(catalogHeader, grant.root)
	}
}

// The client can ask only for a chunk of its advertised manifest or an object
// reachable from it. An old primary rejects this envelope (no v6 protocol),
// without interpreting a download as a configuration or receipt transaction.
type catalogPull struct {
	Root      string `json:"root"`
	Digest    string `json:"sha"`
	Offset    int    `json:"offset,omitempty"`
	ReplicaID string `json:"replica_id,omitempty"`
}

func (m *Manager) serveCatalog(c *gin.Context, peer *Peer) {
	source := &m.catalog
	select {
	case source.pulls <- struct{}{}:
		defer func() { <-source.pulls }()
	default:
		c.AbortWithStatus(http.StatusTooManyRequests)
		return
	}
	var req catalogPull
	if decodeLimited(c.Request.Body, &req, 512) != nil || !validCatalogDigest(req.Root) || !validCatalogDigest(req.Digest) || req.Offset < 0 || req.Offset%catalogChunkBytes != 0 || !ValidReplicaID(req.ReplicaID) {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	source.mu.Lock()
	grant := &m.peers[peer.ID].catalog
	entitled := func() bool {
		return source.image != nil && source.image.root == req.Root && grant.root == req.Root && grant.replica == req.ReplicaID && m.now().Before(source.validUntil)
	}
	ok := entitled()
	source.mu.Unlock()
	if !ok {
		c.AbortWithStatus(http.StatusGone)
		return
	}
	allowed := m.allowCatalog(c.Request.Context())
	source.mu.Lock()
	var data []byte
	if entitled() {
		if !allowed {
			grant.check++
			grant.root = ""
		} else if req.Digest == req.Root {
			data = source.image.manifest
		} else {
			data = source.image.blobs[req.Digest]
		}
	}
	source.mu.Unlock()
	if req.Offset >= len(data) {
		c.AbortWithStatus(http.StatusGone)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header(catalogSizeHeader, strconv.Itoa(len(data)))
	chunk := data[req.Offset:min(req.Offset+catalogChunkBytes, len(data))]
	c.Header("Content-Length", strconv.Itoa(len(chunk)))
	c.Data(http.StatusOK, "application/octet-stream", chunk)
}
