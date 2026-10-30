//go:build unit

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestUserPlatformConcurrencyConcurrentPolicyRefresh(t *testing.T) {
	key := &APIKey{User: &User{ID: 7, Concurrency: 10}, Group: &Group{Platform: PlatformOpenAI}}
	ctx := WithUserConcurrencyPolicy(context.Background(), key, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if i%2 == 0 {
					UpdateUserConcurrencyPolicy(ctx, &User{ID: 7, Concurrency: 10, PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: 2}})
				} else {
					platform, total, limit, err := userConcurrencyLimits(ctx, 7, 99)
					if err != nil || platform != PlatformOpenAI || total != 10 || (limit != 0 && limit != 2) {
						t.Errorf("invalid concurrent policy: %s %d %d %v", platform, total, limit, err)
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

type platformConcurrencyCacheStub struct {
	stubConcurrencyCacheForTest
	platform               string
	total, limit, releases int
}

func (c *platformConcurrencyCacheStub) AcquireUserPlatformSlot(_ context.Context, _ int64, platform string, total, limit int, _ string) (bool, error) {
	c.platform, c.total, c.limit = platform, total, limit
	return true, nil
}
func (c *platformConcurrencyCacheStub) ReleaseUserPlatformSlot(context.Context, int64, string, string) error {
	c.releases++
	return nil
}

func TestUserPlatformConcurrencyPolicyRoutingAndRefresh(t *testing.T) {
	user := &User{ID: 7, Concurrency: 10, PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: 2, PlatformGemini: 3, PlatformAntigravity: 1}}
	key := &APIKey{User: user, Group: &Group{Platform: PlatformComposite}}
	ctx := WithUserConcurrencyPolicy(context.Background(), key, func(ctx context.Context) (int, error) {
		UpdateUserConcurrencyPolicy(ctx, &User{ID: 7, Concurrency: 6, PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: 1}})
		return 0, nil
	})
	cache := &platformConcurrencyCacheStub{}
	svc := NewConcurrencyService(cache)
	_, err := svc.AcquireUserSlot(ctx, 7, 99)
	require.Error(t, err)
	ctx = WithResolvedTargetPlatform(ctx, PlatformOpenAI)
	user.PlatformConcurrencyLimits[PlatformOpenAI] = 99 // policy owns its snapshot
	result, err := svc.AcquireUserSlot(ctx, 7, 99)
	require.NoError(t, err)
	require.True(t, result.Acquired)
	require.Equal(t, PlatformOpenAI, cache.platform)
	require.Equal(t, 10, cache.total)
	require.Equal(t, 2, cache.limit)
	result.ReleaseFunc()
	result.ReleaseFunc()
	require.Equal(t, 1, cache.releases)
	forced := context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAntigravity)
	result, err = svc.AcquireUserSlot(forced, 7, 99)
	require.NoError(t, err)
	result.ReleaseFunc()
	require.Equal(t, PlatformAntigravity, cache.platform)
	require.Equal(t, 1, cache.limit)
	require.NoError(t, svc.RevalidateUserConcurrency(ctx))
	result, err = svc.AcquireUserSlot(ctx, 7, 99)
	require.NoError(t, err)
	result.ReleaseFunc()
	require.Equal(t, 6, cache.total)
	require.Equal(t, 1, cache.limit)
	_, err = svc.AcquireUserSlot(ctx, 8, 99)
	require.Error(t, err)
}

func TestUserPlatformConcurrencyRequiresCapableBackend(t *testing.T) {
	ctx := WithUserConcurrencyPolicy(context.Background(), &APIKey{User: &User{ID: 7, PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: 1}}, Group: &Group{Platform: PlatformOpenAI}}, nil)
	svc := NewConcurrencyService(&stubConcurrencyCacheForTest{acquireResult: true})
	_, err := svc.AcquireUserSlot(ctx, 7, 0)
	require.Error(t, err)
}

func TestUserPlatformConcurrencyValidation(t *testing.T) {
	svc := &adminServiceImpl{}
	for _, limits := range []map[string]int{nil, {}, {PlatformOpenAI: 0}, {PlatformGemini: 2}} {
		require.NoError(t, svc.validatePlatformConcurrencyLimits(limits))
	}
	for _, limits := range []map[string]int{{PlatformComposite: 1}, {"unknown": 1}, {PlatformOpenAI: -1}, {PlatformOpenAI: 2147483648}} {
		require.Error(t, svc.validatePlatformConcurrencyLimits(limits))
	}
	svc.cfg = &config.Config{}
	svc.cfg.Runtime.RegionalConcurrencyFile = "regional.json"
	require.Error(t, svc.validatePlatformConcurrencyLimits(map[string]int{PlatformOpenAI: 1}))
	require.NoError(t, svc.validatePlatformConcurrencyLimits(map[string]int{PlatformOpenAI: 0}))
}

func TestUserPlatformConcurrencyAuthSnapshot(t *testing.T) {
	svc := &APIKeyService{}
	key := &APIKey{ID: 1, UserID: 7, User: &User{ID: 7, Concurrency: 10, PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: 2}}}
	snapshot := svc.snapshotFromAPIKey(context.Background(), key)
	key.User.PlatformConcurrencyLimits[PlatformOpenAI] = 99
	require.Equal(t, 2, snapshot.User.PlatformConcurrencyLimits[PlatformOpenAI])
	got := svc.snapshotToAPIKey("test-key", snapshot)
	require.Equal(t, 2, got.User.PlatformConcurrencyLimits[PlatformOpenAI])
	got.User.PlatformConcurrencyLimits[PlatformOpenAI] = 100
	require.Equal(t, 2, snapshot.User.PlatformConcurrencyLimits[PlatformOpenAI])
	_, ok, err := svc.applyAuthCacheEntry("old", &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{Version: 31}})
	require.NoError(t, err)
	require.False(t, ok)
}

func TestUserPlatformConcurrencyAdminUpdateAndInvalidation(t *testing.T) {
	repo := &userRepoStub{user: &User{ID: 7, Role: RoleUser, Status: StatusActive, PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: 2}}}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{userRepo: repo, authCacheInvalidator: invalidator}
	ctx := context.Background()
	_, err := svc.UpdateUser(ctx, 7, &UpdateUserInput{})
	require.NoError(t, err)
	require.Empty(t, invalidator.userIDs)
	require.Equal(t, 2, repo.user.PlatformConcurrencyLimits[PlatformOpenAI])
	_, err = svc.UpdateUser(ctx, 7, &UpdateUserInput{PlatformConcurrencyLimits: map[string]int{PlatformGemini: 3}})
	require.NoError(t, err)
	require.Equal(t, []int64{7}, invalidator.userIDs)
	require.Equal(t, map[string]int{PlatformGemini: 3}, repo.user.PlatformConcurrencyLimits)
	_, err = svc.UpdateUser(ctx, 7, &UpdateUserInput{PlatformConcurrencyLimits: map[string]int{}})
	require.NoError(t, err)
	require.Empty(t, repo.user.PlatformConcurrencyLimits)
	require.Equal(t, []int64{7, 7}, invalidator.userIDs)
	_, err = svc.UpdateUser(ctx, 7, &UpdateUserInput{PlatformConcurrencyLimits: map[string]int{PlatformOpenAI: -1}})
	require.Error(t, err)
	require.Empty(t, repo.user.PlatformConcurrencyLimits)
}
