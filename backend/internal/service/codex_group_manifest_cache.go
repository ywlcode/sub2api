package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"golang.org/x/sync/singleflight"
)

const (
	groupManifestTTL        = 30 * time.Second
	groupManifestMaxEntries = 128
	groupManifestMaxBytes   = 32 << 20
	groupManifestMaxBody    = 8 << 20
)

type groupManifestEntry struct {
	body    []byte
	expires time.Time
}

// Cache descriptor construction when the caller supplies fresh configuration.
// The outer catalog response cache can also avoid configuration reads when the
// invalidation subscription is healthy. This content-based cache remains the
// fallback for callers without that subscription and for degraded catalog reads.
type codexGroupManifestCache struct {
	mu      sync.Mutex
	entries map[[32]byte]groupManifestEntry
	bytes   int
	epoch   uint64
	flight  singleflight.Group
}

func groupManifestKey(platform string, models []string, accounts []Account, group *Group, routes []CompositeModelRoute, routesAvailable bool) ([32]byte, error) {
	// Exclude usage timestamps and object back-references. Include the actual
	// configuration, not only UpdatedAt: group bindings and synced metadata may
	// change independently. Credentials are hashed, never logged or cached here.
	type binding struct {
		GroupID int64
		Models  []string
	}
	type accountConfig struct {
		ID                     int64
		Platform, Type, Status string
		Credentials, Extra     map[string]any
		Groups                 []int64
		Bindings               []binding
		ParentAccountID        *int64
		QuotaDimension         string
	}
	configs := make([]accountConfig, 0, len(accounts))
	for _, account := range accounts {
		config := accountConfig{
			ID: account.ID, Platform: account.Platform, Type: account.Type, Status: account.Status,
			Credentials: account.Credentials, Extra: account.Extra, Groups: account.GroupIDs,
			ParentAccountID: account.ParentAccountID, QuotaDimension: account.QuotaDimension,
		}
		for _, link := range account.AccountGroups {
			config.Bindings = append(config.Bindings, binding{link.GroupID, link.AllowedModels})
		}
		configs = append(configs, config)
	}
	var groupConfig *Group
	if group != nil {
		copy := *group
		copy.AccountGroups = nil
		groupConfig = &copy
	}
	hash := sha256.New()
	err := json.NewEncoder(hash).Encode(struct {
		Platform              string
		Models                []string
		Accounts              []accountConfig
		Group                 *Group
		Routes                []CompositeModelRoute
		RoutesAvailable       bool
		RuntimeMappingVersion uint64
	}{platform, models, configs, groupConfig, routes, routesAvailable, xai.RuntimeModelMappingVersion()})
	var key [32]byte
	copy(key[:], hash.Sum(nil))
	return key, err
}

func (c *codexGroupManifestCache) get(key [32]byte, now time.Time) ([]byte, uint64) {
	c.mu.Lock()
	epoch := c.epoch
	if entry, ok := c.entries[key]; ok {
		if now.Before(entry.expires) {
			c.mu.Unlock()
			// Entries are immutable. Copy large manifests outside the lock so
			// independent hits do not serialize behind response buffer copies.
			return bytes.Clone(entry.body), epoch
		}
		delete(c.entries, key)
		c.bytes -= len(entry.body)
	}
	c.mu.Unlock()
	return nil, epoch
}

func (c *codexGroupManifestCache) store(key [32]byte, body []byte, epoch uint64, now time.Time) {
	if len(body) == 0 || len(body) > groupManifestMaxBody {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return
	}
	if c.entries == nil {
		c.entries = make(map[[32]byte]groupManifestEntry)
	}
	for k, entry := range c.entries {
		if !now.Before(entry.expires) || k == key {
			delete(c.entries, k)
			c.bytes -= len(entry.body)
		}
	}
	for len(c.entries) >= groupManifestMaxEntries || c.bytes+len(body) > groupManifestMaxBytes {
		var oldestKey [32]byte
		var oldest time.Time
		for k, entry := range c.entries {
			if oldest.IsZero() || entry.expires.Before(oldest) {
				oldestKey, oldest = k, entry.expires
			}
		}
		c.bytes -= len(c.entries[oldestKey].body)
		delete(c.entries, oldestKey)
	}
	c.entries[key] = groupManifestEntry{bytes.Clone(body), now.Add(groupManifestTTL)}
	c.bytes += len(body)
}

func (c *codexGroupManifestCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.bytes = 0
	c.epoch++
}

func (c *codexGroupManifestCache) build(ctx context.Context, key [32]byte, build func() ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, epoch := c.get(key, time.Now())
	if body != nil {
		return body, nil
	}
	result, err, _ := c.flight.Do(fmt.Sprintf("%d:%x", epoch, key), func() (any, error) {
		if body, _ := c.get(key, time.Now()); body != nil {
			return body, nil
		}
		body, err := build()
		if err == nil {
			c.store(key, body, epoch, time.Now())
		}
		return body, err
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return bytes.Clone(result.([]byte)), nil
}

func (s *GatewayService) cachedCodexGroupManifest(ctx context.Context, platform string, models []string, accounts []Account, group *Group, routes []CompositeModelRoute, routesAvailable bool) ([]byte, error) {
	build := func() ([]byte, error) {
		return buildCodexModelsManifestForAccounts(platform, models, accounts, group, routes, routesAvailable)
	}
	key, err := groupManifestKey(platform, models, accounts, group, routes, routesAvailable)
	if err != nil {
		return build()
	}
	return s.codexGroupManifestCache.build(ctx, key, build)
}
