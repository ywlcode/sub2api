package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The sync loop alone chooses a download for the lease. The single optional
// worker owns staging and can publish only into this specific download, never
// into the manager's current lease. A revoked in-flight response is harmless.
type catalogDownload struct {
	root, replica string
	ctx           context.Context
	cancel        context.CancelFunc
	ready         atomic.Pointer[catalogContents]

	reuse      map[string][]byte
	reuseUntil time.Time
	index      *catalogManifest
	blobs      map[string][]byte
	next       int
	partial    []byte
}

// Only the sync loop owns this cache. It has no group index and cannot serve
// requests. The next manifest decides which verified objects are reachable.
type catalogBlobCache struct {
	primary, replica string
	expires          time.Time
	blobs            map[string][]byte
}

func (m *Manager) hideCatalog() {
	if l := m.current.Load(); l != nil && l.catalog != nil {
		m.current.Store(&lease{expires: l.expires})
	}
}

func (m *Manager) catalogView(old *catalogSnapshot, root, replica string, sameVersion bool) *catalogSnapshot {
	if old != nil && old.download.root == root {
		if sameVersion {
			return old
		}
		return &catalogSnapshot{old.download}
	}
	if root == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(m.catalog.ctx)
	d := &catalogDownload{root: root, replica: replica, ctx: ctx, cancel: cancel}
	if m.catalogCache.primary == m.primaryID && m.catalogCache.replica == replica {
		d.reuse = m.catalogCache.blobs
		d.reuseUntil = m.catalogCache.expires
	}
	return &catalogSnapshot{d}
}

func (m *Manager) pullCatalog(view *catalogSnapshot) {
	if view == nil || view.download.ready.Load() != nil {
		return
	}
	c := &m.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.working || c.ctx.Err() != nil {
		return
	}
	c.startLocked(func() {
		// Allow another batch only after another successful config heartbeat.
		d := view.download
		ctx, cancel := context.WithTimeout(d.ctx, 4*time.Second)
		defer cancel()
		_ = d.advance(ctx, m)
	})
}

var errCatalogChunk = errors.New("invalid site catalog chunk")

func (m *Manager) catalogChunk(ctx context.Context, pull catalogPull, expected, limit int) ([]byte, int, error) {
	raw, err := json.Marshal(pull)
	if err != nil {
		return nil, 0, err
	}
	url := strings.TrimRight(m.cfg.PrimaryURL, "/") + SyncPath + "?" + catalogQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.catalogClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, &siteHTTPError{path: SyncPath + "?" + catalogQuery, status: resp.StatusCode}
	}
	size, err := strconv.Atoi(resp.Header.Get(catalogSizeHeader))
	if err != nil || size <= pull.Offset || size > limit || expected != 0 && size != expected || resp.Header.Get("Content-Type") != "application/octet-stream" {
		return nil, 0, errCatalogChunk
	}
	n := min(size-pull.Offset, catalogChunkBytes)
	chunk, err := io.ReadAll(io.LimitReader(resp.Body, int64(n)+1))
	if err != nil {
		return nil, 0, err
	}
	if len(chunk) != n {
		return nil, 0, errCatalogChunk
	}
	return chunk, size, nil
}

func (d *catalogDownload) advance(ctx context.Context, m *Manager) error {
	if d.reuse != nil && !m.now().Before(d.reuseUntil) {
		d.reuse = nil
	}
	for requests := 0; ; requests++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest, expected, limit := d.root, cap(d.partial), catalogMaxManifest
		if d.index != nil {
			for d.next < len(d.index.Objects) && d.blobs[d.index.Objects[d.next].Digest] != nil {
				d.next++
			}
			if d.next == len(d.index.Objects) {
				d.publish()
				return nil
			}
			b := d.index.Objects[d.next]
			digest, expected, limit = b.Digest, b.Size, catalogMaxObject
		}
		if requests == catalogChunksPerRound {
			return nil
		}
		chunk, size, err := m.catalogChunk(ctx, catalogPull{d.root, digest, len(d.partial), d.replica}, expected, limit)
		if err != nil {
			if errors.Is(err, errCatalogChunk) {
				d.partial = nil // even the previously advertised object length may be corrupt
			}
			var status *siteHTTPError
			if errors.As(err, &status) && (status.status == http.StatusGone || status.status == http.StatusForbidden || status.status == http.StatusUnauthorized) {
				d.index, d.blobs, d.reuse, d.partial, d.next = nil, nil, nil, nil, 0
			}
			return err
		}
		if d.partial == nil {
			d.partial = make([]byte, 0, size)
		}
		d.partial = append(d.partial, chunk...)
		if len(d.partial) < size {
			continue
		}
		body := d.partial
		d.partial = nil
		if catalogDigest(body) != digest {
			return fmt.Errorf("%w: digest mismatch", errCatalogChunk)
		}
		if d.index == nil {
			var index catalogManifest
			if json.Unmarshal(body, &index) != nil || !index.valid() {
				return errCatalogChunk
			}
			d.index, d.blobs = &index, make(map[string][]byte, len(index.Objects))
			for _, b := range index.Objects {
				if known := d.reuse[b.Digest]; len(known) == b.Size {
					d.blobs[b.Digest] = known
				}
			}
			d.reuse = nil // drop every object unreachable from the new root
		} else {
			d.blobs[digest] = body
			d.next++
		}
	}
}

func (d *catalogDownload) publish() {
	body := func(ref int64) []byte {
		if ref == 0 {
			return nil
		}
		return d.blobs[d.index.Objects[ref-1].Digest]
	}
	groups := make(map[int64]CatalogGroup, len(d.index.Groups))
	for _, row := range d.index.Groups {
		groups[row[0]] = CatalogGroup{Plain: body(row[1]), Union: body(row[2]), Codex: body(row[3]), Policy: body(row[4])}
	}
	d.ready.Store(&catalogContents{groups: groups, blobs: d.blobs})
	d.index, d.blobs = nil, nil
}
