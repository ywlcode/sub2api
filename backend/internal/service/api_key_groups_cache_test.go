//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type requestGroupSubscriptionRepo struct {
	UserSubscriptionRepository
	subs   map[int64]*UserSubscription
	reads  atomic.Int64
	err    error
	resets int
}

func (r *requestGroupSubscriptionRepo) GetActiveByUserIDAndGroupID(_ context.Context, _, groupID int64) (*UserSubscription, error) {
	r.reads.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	if sub := r.subs[groupID]; sub != nil {
		cp := *sub
		return &cp, nil
	}
	return nil, ErrSubscriptionNotFound
}

func (r *requestGroupSubscriptionRepo) GetByID(ctx context.Context, id int64) (*UserSubscription, error) {
	return r.GetActiveByUserIDAndGroupID(ctx, 7, id)
}

func (r *requestGroupSubscriptionRepo) ResetUsageWindows(_ context.Context, id int64, daily, weekly, monthly bool, dailyStart, periodicStart time.Time) error {
	r.resets++
	sub := r.subs[id]
	if daily {
		sub.DailyUsageUSD, sub.DailyWindowStart = 0, &dailyStart
	}
	if weekly {
		sub.WeeklyUsageUSD, sub.WeeklyWindowStart = 0, &periodicStart
	}
	if monthly {
		sub.MonthlyUsageUSD, sub.MonthlyWindowStart = 0, &periodicStart
	}
	return nil
}

func (r *requestGroupSubscriptionRepo) ResetDailyUsage(ctx context.Context, id int64, _ *time.Time, start time.Time) error {
	return r.ResetUsageWindows(ctx, id, true, false, false, start, start)
}

func (r *requestGroupSubscriptionRepo) ResetWeeklyUsage(ctx context.Context, id int64, _ *time.Time, start time.Time) error {
	return r.ResetUsageWindows(ctx, id, false, true, false, start, start)
}

func (r *requestGroupSubscriptionRepo) ResetMonthlyUsage(ctx context.Context, id int64, _ *time.Time, start time.Time) error {
	return r.ResetUsageWindows(ctx, id, false, false, true, start, start)
}

type requestGroupBillingCache struct {
	BillingCache
	mu      sync.Mutex
	entries map[int64]SubscriptionCacheData
	err     error
	writes  chan struct{}
}

func (c *requestGroupBillingCache) GetSubscriptionCache(_ context.Context, _, groupID int64) (*SubscriptionCacheData, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if data, ok := c.entries[groupID]; ok {
		return &data, nil
	}
	return nil, nil
}

func (c *requestGroupBillingCache) SetSubscriptionCache(_ context.Context, _, groupID int64, data *SubscriptionCacheData) error {
	c.mu.Lock()
	c.entries[groupID] = *data
	c.mu.Unlock()
	select {
	case c.writes <- struct{}{}:
	default:
	}
	return nil
}

func (c *requestGroupBillingCache) InvalidateSubscriptionCache(_ context.Context, _, groupID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, groupID)
	return nil
}

func TestAPIKeyMultiGroupSelectionUsesSubscriptionCaches(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	limit := 10.0
	primary := int64(1)
	groups := map[int64]*Group{
		1: {ID: 1, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &limit, WeeklyLimitUSD: &limit, MonthlyLimitUSD: &limit},
		2: {ID: 2, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &limit},
		3: {ID: 3, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard},
	}
	repo := &requestGroupSubscriptionRepo{subs: map[int64]*UserSubscription{}}
	cache := &requestGroupBillingCache{entries: map[int64]SubscriptionCacheData{}, writes: make(chan struct{}, 16)}
	billing := NewBillingCacheService(cache, nil, repo, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	subscriptions := &SubscriptionService{userSubRepo: repo, billingCacheService: billing, now: func() time.Time { return now }}
	subscriptions.initSubCache(&config.Config{SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 1024, L1TTLSeconds: 60}})
	t.Cleanup(subscriptions.subCacheL1.Close)
	for _, id := range []int64{1, 2} {
		repo.subs[id] = &UserSubscription{ID: id, UserID: 7, GroupID: id, Status: SubscriptionStatusActive,
			ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
		_, err := subscriptions.GetActiveSubscription(ctx, 7, id)
		require.NoError(t, err)
		cache.entries[id] = SubscriptionCacheData{Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)}
	}
	subscriptions.subCacheL1.Wait()
	require.EqualValues(t, 2, repo.reads.Load(), "only cold metadata loads should reach the repository")
	repo.reads.Store(0)
	key := &APIKey{UserID: 7, GroupID: &primary, GroupIDs: []int64{1, 2, 3}, Group: groups[1], User: &User{ID: 7, Balance: 10}}
	// No subscription repository on APIKeyService: routing must reuse SubscriptionService.
	keys := &APIKeyService{groupRepo: multiGroupRepoStub{groups: groups},
		groupRouter: multiGroupRouterStub{1: {Available: true}, 2: {Available: true}, 3: {Available: true}}}
	selectGroup := func(want int64) {
		t.Helper()
		got, err := keys.SelectRequestGroup(ctx, key, nil, "token", subscriptions, nil, true)
		require.NoError(t, err)
		require.Equal(t, want, *got.GroupID)
		require.Same(t, groups[want], got.Group)
	}
	setUsage := func(id int64, daily, weekly, monthly float64) {
		t.Helper()
		require.NoError(t, cache.SetSubscriptionCache(ctx, 7, id, &SubscriptionCacheData{
			Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour),
			DailyUsage: daily, WeeklyUsage: weekly, MonthlyUsage: monthly,
		}))
	}
	for range 20 {
		selectGroup(1)
	}
	// A cached identity must still be active, including on free/read requests.
	for _, inactive := range []UserSubscription{
		{Status: SubscriptionStatusSuspended, ExpiresAt: now.Add(time.Hour)},
		{Status: SubscriptionStatusActive, ExpiresAt: now.Add(-time.Hour)},
	} {
		cp := *repo.subs[1]
		cp.Status, cp.ExpiresAt = inactive.Status, inactive.ExpiresAt
		require.True(t, subscriptions.subCacheL1.SetWithTTL(subCacheKey(7, 1), &cp, 1, time.Minute))
		subscriptions.subCacheL1.Wait()
		got, err := keys.SelectRequestGroup(ctx, key, nil, "token", subscriptions, nil, false)
		require.NoError(t, err)
		require.Equal(t, int64(2), *got.GroupID)
	}
	cp := *repo.subs[1]
	require.True(t, subscriptions.subCacheL1.SetWithTTL(subCacheKey(7, 1), &cp, 1, time.Minute))
	subscriptions.subCacheL1.Wait()
	// L1 still contains zero usage, but the billing cache reflects completed charges.
	for _, usage := range [][3]float64{{limit, 0, 0}, {0, limit, 0}, {0, 0, limit}} {
		setUsage(1, usage[0], usage[1], usage[2])
		selectGroup(2)
	}
	setUsage(2, limit, 0, 0)
	selectGroup(3)
	setUsage(1, 0, 0, 0)
	selectGroup(1)
	require.Zero(t, repo.reads.Load(), "warm routing, quota exhaustion and refreshed quota must not reload subscriptions")
	// A stale exhausted L1 counter must not override a refreshed billing counter.
	repo.subs[1].DailyUsageUSD = limit + 1
	subscriptions.InvalidateSubCacheSync(7, 1)
	_, err := subscriptions.GetActiveSubscription(ctx, 7, 1)
	require.NoError(t, err)
	subscriptions.subCacheL1.Wait()
	repo.reads.Store(0)
	selectGroup(1)
	require.Zero(t, repo.reads.Load())
	// Cache loss refills from the existing DB-backed billing loader.
	repo.subs[1].DailyUsageUSD = 0
	require.NoError(t, cache.InvalidateSubscriptionCache(ctx, 7, 1))
	for len(cache.writes) > 0 {
		<-cache.writes
	}
	selectGroup(1)
	select {
	case <-cache.writes:
	case <-time.After(time.Second):
		t.Fatal("billing cache was not repopulated after a miss")
	}
	require.EqualValues(t, 1, repo.reads.Load())
	selectGroup(1)
	require.EqualValues(t, 1, repo.reads.Load(), "a repopulated billing cache must avoid another DB read")
	// Redis failure falls back to the database, rather than trusting stale L1 usage.
	cache.mu.Lock()
	cache.err = errors.New("redis unavailable")
	cache.mu.Unlock()
	repo.subs[1].DailyUsageUSD = limit
	repo.subs[2].DailyUsageUSD = 0
	selectGroup(2)
	require.EqualValues(t, 3, repo.reads.Load())
	// If both sources fail, do not silently switch to a balance group.
	repo.err = errors.New("database unavailable")
	_, err = keys.SelectRequestGroup(ctx, key, nil, "token", subscriptions, nil, true)
	require.ErrorIs(t, err, ErrBillingServiceUnavailable)
}

func TestAPIKeyMultiGroupSelectionUsesRenewedQuotaAfterAutomaticAndAdminReset(t *testing.T) {
	for _, mode := range []string{"automatic", "admin"} {
		for _, window := range []string{"daily", "weekly", "monthly"} {
			t.Run(mode+"/"+window, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now()
				oldStart := now.AddDate(0, 0, -31)
				limit := 10.0
				primary := int64(1)
				group := &Group{ID: 1, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription,
					DailyLimitUSD: &limit, WeeklyLimitUSD: &limit, MonthlyLimitUSD: &limit}
				sub := &UserSubscription{ID: 1, UserID: 7, GroupID: 1, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour),
					DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
				used := SubscriptionCacheData{Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)}
				switch window {
				case "daily":
					sub.DailyUsageUSD, used.DailyUsage = limit, limit
					if mode == "automatic" {
						sub.DailyWindowStart = &oldStart
					}
				case "weekly":
					sub.WeeklyUsageUSD, used.WeeklyUsage = limit, limit
					if mode == "automatic" {
						sub.WeeklyWindowStart = &oldStart
					}
				case "monthly":
					sub.MonthlyUsageUSD, used.MonthlyUsage = limit, limit
					if mode == "automatic" {
						sub.MonthlyWindowStart = &oldStart
					}
				}
				repo := &requestGroupSubscriptionRepo{subs: map[int64]*UserSubscription{1: sub}}
				cache := &requestGroupBillingCache{entries: map[int64]SubscriptionCacheData{1: used}, writes: make(chan struct{}, 1)}
				billing := NewBillingCacheService(cache, nil, repo, nil, nil, nil, &config.Config{}, nil)
				t.Cleanup(billing.Stop)
				subscriptions := &SubscriptionService{userSubRepo: repo, billingCacheService: billing, now: func() time.Time { return now }}
				subscriptions.initSubCache(&config.Config{SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 1024, L1TTLSeconds: 60}})
				t.Cleanup(subscriptions.subCacheL1.Close)
				_, err := subscriptions.GetActiveSubscription(ctx, 7, 1)
				require.NoError(t, err)
				subscriptions.subCacheL1.Wait()
				keys := &APIKeyService{groupRepo: multiGroupRepoStub{groups: map[int64]*Group{1: group,
					2: {ID: 2, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard}}}, groupRouter: multiGroupRouterStub{1: {Available: true}, 2: {Available: true}}}
				key := &APIKey{UserID: 7, GroupID: &primary, GroupIDs: []int64{1, 2}, User: &User{ID: 7, Balance: 10}}
				selectGroup := func(want int64) {
					t.Helper()
					got, err := keys.SelectRequestGroup(ctx, key, nil, "token", subscriptions, nil, true)
					require.NoError(t, err)
					require.Equal(t, want, *got.GroupID)
				}
				if mode == "admin" {
					selectGroup(2)
					_, err := subscriptions.AdminResetQuota(ctx, 1, window == "daily", window == "weekly", window == "monthly")
					require.NoError(t, err)
					_, cached := subscriptions.subCacheL1.Get(subCacheKey(7, 1))
					require.False(t, cached, "admin reset must invalidate L1 synchronously")
					data, err := cache.GetSubscriptionCache(ctx, 7, 1)
					require.NoError(t, err)
					require.Nil(t, data, "admin reset must invalidate billing usage")
				}
				selectGroup(1)
				require.Equal(t, 1, repo.resets)
				select {
				case <-cache.writes:
				case <-time.After(time.Second):
					t.Fatal("billing cache was not repopulated after reset")
				}
				// Usage already charged to the new window must not be cleared again.
				require.NoError(t, cache.SetSubscriptionCache(ctx, 7, 1, &used))
				selectGroup(2)
				require.Equal(t, 1, repo.resets)
			})
		}
	}
}
