package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9" //nolint:depguard // This cache adapter owns Redis leases and storage, like the ops cache services.
	"golang.org/x/sync/singleflight"
)

const requestPhaseRefreshInterval = time.Hour
const requestPhaseCacheRetention = 7 * 24 * time.Hour

type requestPhaseReader interface {
	ReadUsageRequestPhaseAverages(context.Context, usagestats.UsageLogFilters) (usagestats.RequestPhaseAverages, error)
}

type globalRequestPhaseHourlyReader interface {
	ReadGlobalRequestPhaseHourlyAverages(context.Context, time.Time, time.Time) (usagestats.RequestPhaseAverages, error)
}

type globalRequestPhaseHourlyWorker interface {
	SeedGlobalRequestPhaseHourBuckets(context.Context, time.Time) error
	RefreshOneGlobalRequestPhaseHourBucket(context.Context, time.Time) (bool, error)
}

type requestPhaseCacheEntry struct {
	UserID     int64                           `json:"user_id,omitempty"`
	APIKeyID   int64                           `json:"api_key_id,omitempty"`
	ComputedAt time.Time                       `json:"computed_at"`
	Stats      usagestats.RequestPhaseAverages `json:"stats"`
}

func (e requestPhaseCacheEntry) scope() usagestats.UsageLogFilters {
	return usagestats.UsageLogFilters{UserID: e.UserID, APIKeyID: e.APIKeyID}
}

// UsageRequestPhaseCache shares hourly diagnostics across page filters and app
// replicas. Freshness is independent of eviction: failed refreshes keep the last
// successful value, and only an absent value makes a page wait for computation.
type UsageRequestPhaseCache struct {
	repo   UsageLogRepository
	rdb    *redis.Client
	prefix string
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	flight singleflight.Group
}

func NewUsageRequestPhaseCache(repo UsageLogRepository, rdb *redis.Client, cfg *config.Config) *UsageRequestPhaseCache {
	prefix := "sub2api:"
	if cfg != nil {
		prefix = strings.TrimSpace(cfg.Dashboard.KeyPrefix)
	}
	if prefix != "" && !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &UsageRequestPhaseCache{repo: repo, rdb: rdb, prefix: prefix + "usage:request-phases:v1:", ctx: ctx, cancel: cancel}
}

func (s *UsageRequestPhaseCache) key(scope usagestats.UsageLogFilters) string {
	if scope.UserID > 0 {
		return fmt.Sprintf("user:%d", scope.UserID)
	}
	if scope.APIKeyID > 0 {
		return fmt.Sprintf("key:%d", scope.APIKeyID)
	}
	// Keep the user/key v1 entries intact while rejecting old global scans.
	return "global-hourly-v2"
}

func (s *UsageRequestPhaseCache) read(ctx context.Context, scope usagestats.UsageLogFilters) (requestPhaseCacheEntry, error) {
	var entry requestPhaseCacheEntry
	readCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	raw, err := s.rdb.Get(readCtx, s.prefix+"value:"+s.key(scope)).Bytes()
	if err != nil {
		return entry, err
	}
	if json.Unmarshal(raw, &entry) != nil || entry.UserID != scope.UserID || entry.APIKeyID != scope.APIKeyID || entry.ComputedAt.IsZero() {
		return requestPhaseCacheEntry{}, redis.Nil
	}
	return entry, nil
}

func (s *UsageRequestPhaseCache) Get(ctx context.Context, filters usagestats.UsageLogFilters) usagestats.RequestPhaseAverages {
	scope := requestPhaseScope(filters)
	if s.rdb == nil {
		return usagestats.RequestPhaseAverages{}
	}
	entry, err := s.read(ctx, scope)
	if err == nil {
		return entry.Stats
	}
	// A cache outage must not send every page to the expensive historical query.
	if !errors.Is(err, redis.Nil) {
		return usagestats.RequestPhaseAverages{}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result := s.flight.DoChan(s.key(scope), func() (any, error) {
		// Finish filling the cache even if the requesting browser leaves. Slow users
		// get a background fill rather than permanently missing the two-second budget.
		return s.compute(s.ctx, scope, false)
	})
	select {
	case <-waitCtx.Done():
		return usagestats.RequestPhaseAverages{}
	case r := <-result:
		if r.Err != nil {
			return usagestats.RequestPhaseAverages{}
		}
		entry, ok := r.Val.(requestPhaseCacheEntry)
		if !ok {
			return usagestats.RequestPhaseAverages{}
		}
		return entry.Stats
	}
}

func (s *UsageRequestPhaseCache) compute(parent context.Context, scope usagestats.UsageLogFilters, refresh bool) (requestPhaseCacheEntry, error) {
	return s.computeWithForce(parent, scope, refresh, false)
}

func (s *UsageRequestPhaseCache) computeWithForce(parent context.Context, scope usagestats.UsageLogFilters, refresh, force bool) (requestPhaseCacheEntry, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	entry, err := s.read(ctx, scope)
	if err == nil && (!refresh || (!force && time.Since(entry.ComputedAt) < requestPhaseRefreshInterval)) {
		return entry, nil
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		return entry, err
	}
	lockKey := s.prefix + "lock:" + s.key(scope)
	owner := uuid.NewString()
	locked, err := s.rdb.SetNX(ctx, lockKey, owner, 30*time.Second).Result()
	if err != nil {
		return entry, err
	}
	if !locked && force {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for !locked {
			select {
			case <-ctx.Done():
				return entry, ctx.Err()
			case <-ticker.C:
				locked, err = s.rdb.SetNX(ctx, lockKey, owner, 30*time.Second).Result()
				if err != nil {
					return entry, err
				}
			}
		}
	}
	if !locked {
		if !entry.ComputedAt.IsZero() {
			return entry, nil
		}
		// Another replica is filling this cold scope. Join its result without issuing
		// a duplicate trace scan. The caller's wait remains capped at two seconds.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return entry, ctx.Err()
			case <-ticker.C:
				entry, err = s.read(ctx, scope)
				if err == nil || !errors.Is(err, redis.Nil) {
					return entry, err
				}
			}
		}
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), time.Second)
		defer unlockCancel()
		_ = s.rdb.Eval(unlockCtx, `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{lockKey}, owner).Err()
	}()
	// Recheck after the lease to close the cache-miss/lease-acquisition race.
	latest, readErr := s.read(ctx, scope)
	if readErr == nil && (!refresh || (!force && time.Since(latest.ComputedAt) < requestPhaseRefreshInterval)) {
		return latest, nil
	}
	if readErr != nil && !errors.Is(readErr, redis.Nil) {
		return entry, readErr
	}
	window := requestPhaseWindow(scope, time.Now())
	var stats usagestats.RequestPhaseAverages
	if scope.UserID == 0 && scope.APIKeyID == 0 {
		reader, ok := s.repo.(globalRequestPhaseHourlyReader)
		if !ok {
			return entry, errors.New("global request phase hourly reader unavailable")
		}
		stats, err = reader.ReadGlobalRequestPhaseHourlyAverages(ctx, *window.StartTime, *window.EndTime)
	} else {
		reader, ok := s.repo.(requestPhaseReader)
		if !ok {
			return entry, errors.New("request phase reader unavailable")
		}
		stats, err = reader.ReadUsageRequestPhaseAverages(ctx, window)
	}
	if err != nil {
		return entry, err
	}
	entry = requestPhaseCacheEntry{UserID: scope.UserID, APIKeyID: scope.APIKeyID, ComputedAt: time.Now().UTC(), Stats: stats}
	raw, err := json.Marshal(entry)
	if err != nil {
		return entry, err
	}
	// Fence a late computation whose lease expired: it must not overwrite a more
	// recent result from another replica.
	err = s.rdb.Eval(ctx, `if redis.call('GET',KEYS[1])==ARGV[1] then redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[3]); return 1 end return 0`,
		[]string{lockKey, s.prefix + "value:" + s.key(scope)}, owner, string(raw), int(requestPhaseCacheRetention.Seconds())).Err()
	return entry, err
}

func (s *UsageRequestPhaseCache) Start() {
	if s.rdb == nil {
		return
	}
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				if err := s.refreshGlobalHourly(s.ctx); err != nil && s.ctx.Err() == nil {
					logger.LegacyPrintf("usage_phase_cache", "global bucket refresh failed: %v", err)
				}
				timer.Reset(time.Minute)
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		var lastGlobal, lastUsers time.Time
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				if time.Since(lastGlobal) >= requestPhaseRefreshInterval {
					if _, err := s.compute(s.ctx, usagestats.UsageLogFilters{}, true); err != nil {
						logger.LegacyPrintf("usage_phase_cache", "global refresh failed: %v", err)
					} else {
						lastGlobal = time.Now()
					}
				}
				if time.Since(lastUsers) >= requestPhaseRefreshInterval {
					if err := s.refreshUsers(s.ctx); err != nil {
						logger.LegacyPrintf("usage_phase_cache", "user refresh failed: %v", err)
					} else {
						lastUsers = time.Now()
					}
				}
				if err := s.refreshCached(s.ctx); err != nil && s.ctx.Err() == nil {
					logger.LegacyPrintf("usage_phase_cache", "cached scope refresh failed: %v", err)
				}
				timer.Reset(time.Minute)
			}
		}
	}()
}

func (s *UsageRequestPhaseCache) refreshGlobalHourly(ctx context.Context) error {
	reader, ok := s.repo.(globalRequestPhaseHourlyWorker)
	if !ok {
		return errors.New("global request phase hourly reader unavailable")
	}
	seedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := reader.SeedGlobalRequestPhaseHourBuckets(seedCtx, time.Now())
	cancel()
	if err != nil {
		return err
	}
	changed := false
	var refreshErr error
	// Limit one pass to one day's buckets even if late traces keep an hour dirty.
	for n := 0; n < 24; n++ {
		bucketCtx, bucketCancel := context.WithTimeout(ctx, 15*time.Second)
		updated, err := reader.RefreshOneGlobalRequestPhaseHourBucket(bucketCtx, time.Now())
		bucketCancel()
		if err != nil {
			refreshErr = err
			break
		}
		if !updated {
			break
		}
		changed = true
	}
	if !changed {
		return refreshErr
	}
	// Force a replacement while retaining the previous successful value on failure.
	_, err = s.computeWithForce(ctx, usagestats.UsageLogFilters{}, true, true)
	return errors.Join(refreshErr, err)
}

func (s *UsageRequestPhaseCache) refreshUsers(ctx context.Context) error {
	reader, ok := s.repo.(interface {
		ListRequestPhaseUserIDs(context.Context, int64, int) ([]int64, error)
	})
	if !ok {
		return nil
	}
	var after int64
	for {
		pageCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ids, err := reader.ListRequestPhaseUserIDs(pageCtx, after, 100)
		cancel()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := s.compute(ctx, usagestats.UsageLogFilters{UserID: id}, true); err != nil {
				logger.LegacyPrintf("usage_phase_cache", "user %d refresh failed: %v", id, err)
			}
			after = id
		}
	}
}

// Also refresh scopes populated by key-only dashboards, and values filled
// between hourly user passes. SCAN is incremental and uses the cache namespace.
func (s *UsageRequestPhaseCache) refreshCached(ctx context.Context) error {
	var cursor uint64
	for {
		scanCtx, cancel := context.WithTimeout(ctx, time.Second)
		keys, next, err := s.rdb.Scan(scanCtx, cursor, s.prefix+"value:*", 100).Result()
		cancel()
		if err != nil {
			return err
		}
		for _, key := range keys {
			readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
			raw, err := s.rdb.Get(readCtx, key).Bytes()
			readCancel()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return err
			}
			var entry requestPhaseCacheEntry
			if json.Unmarshal(raw, &entry) != nil || key != s.prefix+"value:"+s.key(entry.scope()) {
				continue
			}
			if time.Since(entry.ComputedAt) < requestPhaseRefreshInterval {
				continue
			}
			if _, err := s.compute(ctx, entry.scope(), true); err != nil {
				logger.LegacyPrintf("usage_phase_cache", "scope refresh failed: %v", err)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (s *UsageRequestPhaseCache) Stop() {
	s.cancel()
	s.wg.Wait()
}
