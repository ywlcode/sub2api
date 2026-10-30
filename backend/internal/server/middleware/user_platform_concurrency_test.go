//go:build unit

package middleware

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type platformPolicyCache struct {
	service.ConcurrencyCache
	total, limit int
}

func (c *platformPolicyCache) AcquireUserPlatformSlot(_ context.Context, _ int64, _ string, total, limit int, _ string) (bool, error) {
	c.total, c.limit = total, limit
	return true, nil
}
func (*platformPolicyCache) ReleaseUserPlatformSlot(context.Context, int64, string, string) error {
	return nil
}

func TestUserPlatformConcurrencyRevalidatorPublishesFreshLimits(t *testing.T) {
	initial := queueAuthTestKey(service.StatusActive, 0)
	latest := queueAuthTestKey(service.StatusActive, 0)
	latest.User.Concurrency = 10
	latest.User.PlatformConcurrencyLimits = map[string]int{service.PlatformOpenAI: 2}
	revalidate := newAPIKeyAuthRevalidator(func(context.Context, string) (*service.APIKey, error) { return latest, nil }, initial.Key, "", initial)
	ctx := service.WithUserConcurrencyPolicy(context.Background(), initial, revalidate)
	cache := &platformPolicyCache{}
	svc := service.NewConcurrencyService(cache)
	require.NoError(t, svc.RevalidateUserConcurrency(ctx))
	result, err := svc.AcquireUserSlot(ctx, initial.UserID, initial.User.Concurrency)
	require.NoError(t, err)
	result.ReleaseFunc()
	require.Equal(t, 10, cache.total)
	require.Equal(t, 2, cache.limit)
	require.Empty(t, initial.User.PlatformConcurrencyLimits)
	latest = queueAuthTestKey(service.StatusActive, 0)
	require.NoError(t, svc.RevalidateUserConcurrency(ctx))
	result, err = svc.AcquireUserSlot(ctx, initial.UserID, initial.User.Concurrency)
	require.NoError(t, err)
	result.ReleaseFunc()
	require.Zero(t, cache.limit)
	latest.Status = service.StatusDisabled
	require.Error(t, svc.RevalidateUserConcurrency(ctx))
}
