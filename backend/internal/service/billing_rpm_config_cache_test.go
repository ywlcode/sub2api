//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBillingRPMConfigCacheRetainsNilAndLiveCounters(t *testing.T) {
	ctx := context.Background()
	repo := &rpmOverrideRepoStub{}
	counters := &userRPMCacheStub{userGroupCounts: []int{1, 2, 3}}
	billing := newBillingServiceForRPM(t, counters, repo)
	keys := &APIKeyService{userGroupRateRepo: repo, cache: &requestConfigAuthCacheStub{}}
	keys.authInvalidationConnected.Store(true)
	keys.connectBillingRPMCache(billing)
	user, group := &User{ID: 1, RPMLimit: 100}, &Group{ID: 2, RPMLimit: 2}
	require.NoError(t, billing.checkRPM(ctx, user, group))
	require.NoError(t, billing.checkRPM(ctx, user, group))
	require.ErrorIs(t, billing.checkRPM(ctx, user, group), ErrGroupRPMExceeded)
	require.Equal(t, int32(1), atomic.LoadInt32(&repo.calls))
	require.Equal(t, int32(3), atomic.LoadInt32(&counters.userGroupCalls))

	zero := 0
	repo.override = &zero
	keys.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "user:1")
	require.NoError(t, billing.checkRPM(ctx, user, group))
	require.Equal(t, int32(3), atomic.LoadInt32(&counters.userGroupCalls), "explicit zero exempts only the group counter")
	require.Equal(t, int32(3), atomic.LoadInt32(&counters.userCalls), "user-wide limits remain live")

	value, err := keys.requestRPMOverride(ctx, 1, 2)
	require.NoError(t, err)
	*value = 999
	value, err = keys.requestRPMOverride(ctx, 1, 2)
	require.NoError(t, err)
	require.Zero(t, *value, "callers must not mutate the cached pointer")
	_, err = keys.requestRPMOverride(ctx, 9, 2)
	require.NoError(t, err)
	_, err = keys.requestRPMOverride(ctx, 1, 9)
	require.NoError(t, err)
	require.Equal(t, int32(4), atomic.LoadInt32(&repo.calls), "cache entries are scoped to both user and group")
}

func TestBillingRPMConfigCacheErrorsExpiryAndReadiness(t *testing.T) {
	ctx := context.Background()
	repo := &rpmOverrideRepoStub{err: errors.New("unavailable")}
	keys := &APIKeyService{userGroupRateRepo: repo, cache: &requestConfigAuthCacheStub{}}
	keys.authInvalidationConnected.Store(true)
	for range 2 {
		_, err := keys.requestRPMOverride(ctx, 1, 2)
		require.Error(t, err)
	}
	require.Equal(t, int32(2), atomic.LoadInt32(&repo.calls))
	repo.err = nil
	_, err := keys.requestRPMOverride(ctx, 1, 2)
	require.NoError(t, err)
	keys.requestRPMOverrides.mu.Lock()
	k := userGroupPolicyKey{1, 2}
	entry := keys.requestRPMOverrides.entries[k]
	entry.expires = time.Now().Add(-time.Second)
	keys.requestRPMOverrides.entries[k] = entry
	keys.requestRPMOverrides.mu.Unlock()
	_, err = keys.requestRPMOverride(ctx, 1, 2)
	require.NoError(t, err)
	require.Equal(t, int32(4), atomic.LoadInt32(&repo.calls))
	keys.authInvalidationConnected.Store(false)
	for range 2 {
		_, err = keys.requestRPMOverride(ctx, 1, 2)
		require.NoError(t, err)
	}
	require.Equal(t, int32(6), atomic.LoadInt32(&repo.calls), "unhealthy invalidation subscription bypasses caching")
}

func TestBillingRPMConfigCacheReplicaInvalidation(t *testing.T) {
	bus := &configNotificationBus{}
	repo := &rpmOverrideRepoStub{}
	a := &APIKeyService{cache: bus, userGroupRateRepo: repo}
	b := &APIKeyService{cache: bus, userGroupRateRepo: repo}
	for _, svc := range []*APIKeyService{a, b} {
		svc.StartAuthCacheInvalidationSubscriber(context.Background())
		defer svc.StopAuthCacheInvalidationSubscriber()
	}
	require.Eventually(t, func() bool { return a.authInvalidationConnected.Load() && b.authInvalidationConnected.Load() }, time.Second, time.Millisecond)
	for _, svc := range []*APIKeyService{a, b} {
		v, err := svc.requestRPMOverride(context.Background(), 1, 2)
		require.NoError(t, err)
		require.Nil(t, v)
	}
	limit := 12
	repo.override = &limit
	a.invalidateRequestConfig("group", 2)
	for _, svc := range []*APIKeyService{a, b} {
		v, err := svc.requestRPMOverride(context.Background(), 1, 2)
		require.NoError(t, err)
		require.Equal(t, 12, *v)
	}
	require.Equal(t, int32(4), atomic.LoadInt32(&repo.calls))
}
