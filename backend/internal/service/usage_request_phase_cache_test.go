package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9" //nolint:depguard // Exercise the cache adapter against isolated miniredis.
	"github.com/stretchr/testify/require"
)

type phaseCacheTestRepo struct {
	UsageLogRepository
	mu    sync.Mutex
	reads []usagestats.UsageLogFilters
	err   error
	users []int64
	gate  chan struct{}
}

func (r *phaseCacheTestRepo) ReadUsageRequestPhaseAverages(ctx context.Context, f usagestats.UsageLogFilters) (usagestats.RequestPhaseAverages, error) {
	r.mu.Lock()
	r.reads = append(r.reads, f)
	err := r.err
	r.mu.Unlock()
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return usagestats.RequestPhaseAverages{}, ctx.Err()
		}
	}
	value := float64(f.UserID*100 + f.APIKeyID)
	return usagestats.RequestPhaseAverages{AverageUserUploadMs: &value, AverageProcessingMs: &value, TimingSampleCount: 1}, err
}

func (r *phaseCacheTestRepo) ListRequestPhaseUserIDs(_ context.Context, after int64, _ int) ([]int64, error) {
	var ids []int64
	for _, id := range r.users {
		if id > after {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func newPhaseCacheTest(t *testing.T, repo *phaseCacheTestRepo) *UsageRequestPhaseCache {
	t.Helper()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	cache := NewUsageRequestPhaseCache(repo, rdb, nil)
	t.Cleanup(func() { cache.Stop(); _ = rdb.Close() })
	return cache
}

func TestRequestPhaseCacheScopeAnd24HourWindow(t *testing.T) {
	repo := &phaseCacheTestRepo{}
	cache := newPhaseCacheTest(t, repo)
	ctx := context.Background()
	start := time.Now().Add(-7 * 24 * time.Hour)
	first := cache.Get(ctx, usagestats.UsageLogFilters{UserID: 4, APIKeyID: 7, Model: "filtered", StartTime: &start})
	require.Equal(t, 400.0, *first.AverageUserUploadMs)
	require.Equal(t, first, cache.Get(ctx, usagestats.UsageLogFilters{UserID: 4}))
	user := cache.Get(ctx, usagestats.UsageLogFilters{UserID: 5})
	key := cache.Get(ctx, usagestats.UsageLogFilters{APIKeyID: 7})
	require.Equal(t, 500.0, *user.AverageUserUploadMs)
	require.Equal(t, 7.0, *key.AverageUserUploadMs)
	global := cache.Get(ctx, usagestats.UsageLogFilters{})
	require.EqualValues(t, 1, global.TimingSampleCount)
	require.Equal(t, 0.0, *global.AverageUserUploadMs)
	require.Equal(t, "global", cache.key(usagestats.UsageLogFilters{}))
	require.Len(t, repo.reads, 4)
	for _, f := range repo.reads {
		require.Empty(t, f.Model)
		require.Equal(t, 24*time.Hour, f.EndTime.Sub(*f.StartTime))
		require.WithinDuration(t, time.Now(), *f.EndTime, 31*time.Second)
	}
	require.Zero(t, repo.reads[0].APIKeyID, "user page filters do not split the shared per-user cache")
	require.Zero(t, repo.reads[2].UserID, "key-only credentials cannot see their owner's other requests")
	require.Zero(t, repo.reads[3].UserID)
	require.Zero(t, repo.reads[3].APIKeyID)
}

func TestRequestPhaseCacheRefreshesGlobalAverage(t *testing.T) {
	repo := &phaseCacheTestRepo{}
	cache := newPhaseCacheTest(t, repo)
	ctx := context.Background()
	scope := usagestats.UsageLogFilters{}
	require.EqualValues(t, 1, cache.Get(ctx, scope).TimingSampleCount)
	entry, err := cache.read(ctx, scope)
	require.NoError(t, err)
	entry.ComputedAt = time.Now().Add(-2 * time.Hour)
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, cache.rdb.Set(ctx, cache.prefix+"value:"+cache.key(scope), raw, requestPhaseCacheRetention).Err())
	require.NoError(t, cache.refreshCached(ctx))
	require.Len(t, repo.reads, 2)
	refreshed, err := cache.read(ctx, scope)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), refreshed.ComputedAt, time.Second)
}

func TestRequestPhaseCacheConcurrentMissAcrossReplicas(t *testing.T) {
	repo := &phaseCacheTestRepo{gate: make(chan struct{})}
	first := newPhaseCacheTest(t, repo)
	second := NewUsageRequestPhaseCache(repo, first.rdb, nil)
	t.Cleanup(second.Stop)
	scope := usagestats.UsageLogFilters{UserID: 3}
	results := make(chan usagestats.RequestPhaseAverages, 10)
	for i := 0; i < 10; i++ {
		c := first
		if i%2 == 0 {
			c = second
		}
		go func() { results <- c.Get(context.Background(), scope) }()
	}
	require.Eventually(t, func() bool { repo.mu.Lock(); defer repo.mu.Unlock(); return len(repo.reads) > 0 }, time.Second, time.Millisecond)
	close(repo.gate)
	for i := 0; i < 10; i++ {
		require.EqualValues(t, 1, (<-results).TimingSampleCount)
	}
	require.Len(t, repo.reads, 1)
}

func TestRequestPhaseCacheRefreshRetainsSuccessAndWarmsEveryUser(t *testing.T) {
	repo := &phaseCacheTestRepo{users: []int64{1, 2, 3}}
	cache := newPhaseCacheTest(t, repo)
	ctx := context.Background()
	require.NoError(t, cache.refreshUsers(ctx))
	require.Len(t, repo.reads, 3)
	require.NoError(t, cache.refreshUsers(ctx))
	require.Len(t, repo.reads, 3, "fresh users must not be recomputed")
	scope := usagestats.UsageLogFilters{UserID: 1}
	entry, err := cache.read(ctx, scope)
	require.NoError(t, err)
	entry.ComputedAt = time.Now().Add(-2 * time.Hour)
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, cache.rdb.Set(ctx, cache.prefix+"value:"+cache.key(scope), raw, requestPhaseCacheRetention).Err())
	require.Equal(t, entry.Stats, cache.Get(ctx, scope))
	require.Len(t, repo.reads, 3, "stale values return without running a page query")
	repo.err = errors.New("temporary database failure")
	require.NoError(t, cache.refreshCached(ctx))
	require.Equal(t, entry.Stats, cache.Get(ctx, scope))
	failedEntry, err := cache.read(ctx, scope)
	require.NoError(t, err)
	require.True(t, entry.ComputedAt.Equal(failedEntry.ComputedAt))
	repo.err = nil
	require.NoError(t, cache.refreshCached(ctx))
	refreshed, err := cache.read(ctx, scope)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), refreshed.ComputedAt, time.Second)
}

func TestRequestPhaseCacheDoesNotCacheFailedColdRead(t *testing.T) {
	repo := &phaseCacheTestRepo{err: errors.New("unavailable")}
	cache := newPhaseCacheTest(t, repo)
	scope := usagestats.UsageLogFilters{APIKeyID: 9}
	require.Zero(t, cache.Get(context.Background(), scope).TimingSampleCount)
	_, err := cache.read(context.Background(), scope)
	require.ErrorIs(t, err, redis.Nil)
	repo.err = nil
	require.EqualValues(t, 1, cache.Get(context.Background(), scope).TimingSampleCount)
}
