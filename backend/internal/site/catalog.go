package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
)

const (
	CatalogPlain  = "plain"
	CatalogUnion  = "union"
	CatalogCodex  = "codex"
	CatalogPolicy = "policy"
)

// CatalogGroup contains one primary rendering, including the exact policy used
// to render it. LoadCatalog supplies the common discovery scope of the paired
// sites. Empty variants are unavailable. Policy is a binary SHA-256, not JSON.
type CatalogGroup struct {
	Plain, Union, Codex, Policy []byte
}

const (
	catalogMaxGroups      = 8192
	catalogMaxObject      = 16 << 20
	catalogMaxManifest    = 4 << 20
	catalogMaxUniqueBytes = 64 << 20
	catalogMaxHashBytes   = 256 << 20
	catalogChunkBytes     = 256 << 10
	catalogChunksPerRound = 8
)

type catalogObject struct {
	Digest string `json:"sha"`
	Size   int    `json:"size"`
}

// Each row is [group ID, plain, union, codex, policy]. Indices into Objects
// are one-based; zero denotes an absent variant. Sorted rows and first-use
// object order give identical publications the same root across processes.
type catalogManifest struct {
	Groups  [][5]int64      `json:"groups"`
	Objects []catalogObject `json:"objects"`
}

type catalogImage struct {
	root     string
	manifest []byte
	blobs    map[string][]byte
}

func catalogDigest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validCatalogDigest(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for _, c := range s {
		digit := c >= '0' && c <= '9'
		lowerHex := c >= 'a' && c <= 'f'
		if !digit && !lowerHex {
			return false
		}
	}
	return true
}

var errCatalogLimit = errors.New("site catalog exceeds publication limit")

func buildCatalog(ctx context.Context, groups map[int64]CatalogGroup) (*catalogImage, error) {
	if len(groups) > catalogMaxGroups {
		return nil, errCatalogLimit
	}
	ids := make([]int64, 0, len(groups))
	for id, g := range groups {
		if id > 0 && len(g.Policy) == sha256.Size {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var index catalogManifest
	image := &catalogImage{blobs: make(map[string][]byte)}
	indices := make(map[string]int64)
	// Renderers often share a large backing slice between groups. Hash that
	// slice once. Equal bytes in different allocations still share one blob.
	type span struct {
		first *byte
		size  int
	}
	spans := make(map[span]int64)
	var unique, hashed int
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g := groups[id]
		row := [5]int64{id}
		for i, b := range [4][]byte{g.Plain, g.Union, g.Codex, g.Policy} {
			if len(b) == 0 || len(b) > catalogMaxObject {
				continue
			}
			if i == 3 && row[1] == 0 && row[2] == 0 && row[3] == 0 {
				break // no usable body for this policy
			}
			key := span{&b[0], len(b)}
			n := spans[key]
			if n == 0 {
				hashed += len(b)
				if hashed > catalogMaxHashBytes {
					return nil, errCatalogLimit
				}
				digest := catalogDigest(b)
				n = indices[digest]
				if n == 0 {
					unique += len(b)
					if unique > catalogMaxUniqueBytes {
						return nil, errCatalogLimit
					}
					image.blobs[digest] = bytes.Clone(b)
					index.Objects = append(index.Objects, catalogObject{digest, len(b)})
					n = int64(len(index.Objects))
					indices[digest] = n
				}
				spans[key] = n
			}
			row[i+1] = n
		}
		if row[4] != 0 {
			index.Groups = append(index.Groups, row)
		}
	}
	if len(index.Groups) == 0 {
		return nil, nil // an authoritative empty catalog withdraws the publication
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if len(raw) > catalogMaxManifest {
		return nil, errCatalogLimit
	}
	image.root, image.manifest = catalogDigest(raw), raw
	return image, nil
}

func (index *catalogManifest) valid() bool {
	if len(index.Groups) == 0 || len(index.Groups) > catalogMaxGroups || len(index.Objects) == 0 || len(index.Objects) > 4*len(index.Groups) {
		return false
	}
	seen := make(map[string]bool, len(index.Objects))
	var total int
	for _, b := range index.Objects {
		if !validCatalogDigest(b.Digest) || seen[b.Digest] || b.Size <= 0 || b.Size > catalogMaxObject {
			return false
		}
		total += b.Size
		if total > catalogMaxUniqueBytes {
			return false
		}
		seen[b.Digest] = true
	}
	var previous int64
	for _, row := range index.Groups {
		if row[0] <= previous || row[4] == 0 || row[1] == 0 && row[2] == 0 && row[3] == 0 {
			return false
		}
		for _, ref := range row[1:] {
			if ref < 0 || ref > int64(len(index.Objects)) {
				return false
			}
		}
		if index.Objects[row[4]-1].Size != sha256.Size {
			return false
		}
		previous = row[0]
	}
	return true
}

type catalogContents struct {
	groups map[int64]CatalogGroup
	blobs  map[string][]byte // verified bytes available for reuse by the next root
}

// A view is valid for a single installed authorization version and catalog
// root. The download publishes its complete immutable contents exactly once.
type catalogSnapshot struct{ download *catalogDownload }

// Get returns immutable bytes, or nil when the group or variant is absent.
// Callers compare CatalogPolicy with the authenticated group's current policy.
func (s *catalogSnapshot) Get(groupID int64, kind string) []byte {
	g := s.download.ready.Load().groups[groupID]
	switch kind {
	case CatalogPlain:
		return g.Plain
	case CatalogUnion:
		return g.Union
	case CatalogCodex:
		return g.Codex
	case CatalogPolicy:
		return g.Policy
	default:
		return nil
	}
}

func (m *Manager) discoverySnapshot() *catalogSnapshot {
	l := m.current.Load()
	if l == nil || l.catalog == nil || l.catalog.download.ready.Load() == nil || !m.now().Before(l.expires) {
		return nil
	}
	return l.catalog
}
