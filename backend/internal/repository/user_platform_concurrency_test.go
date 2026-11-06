package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func platformTestCache(t *testing.T) (*concurrencyCache, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache, ok := NewConcurrencyCache(client, 15, 900).(*concurrencyCache)
	require.True(t, ok)
	return cache, server
}

func TestUserPlatformConcurrencyAtomicLimits(t *testing.T) {
	c, _ := platformTestCache(t)
	ctx := context.Background()
	acquire := func(user int64, platform, id string, total, limit int, want bool) {
		t.Helper()
		got, err := c.AcquireUserPlatformSlot(ctx, user, platform, total, limit, id)
		require.NoError(t, err)
		require.Equal(t, want, got, id)
	}
	acquire(1, "openai", "key-a", 3, 1, true)
	acquire(1, "openai", "key-b", 3, 1, false)
	acquire(1, "anthropic", "other-platform", 3, 2, true)
	acquire(2, "openai", "other-user", 3, 1, true)
	acquire(1, "anthropic", "second", 3, 2, true)
	acquire(1, "gemini", "total-full", 3, 0, false)
	count, err := c.GetUserConcurrency(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	require.NoError(t, c.ReleaseUserPlatformSlot(ctx, 1, "openai", "key-a"))
	require.NoError(t, c.ReleaseUserPlatformSlot(ctx, 1, "openai", "key-a"))
	acquire(1, "openai", "replacement", 3, 1, true)
	acquire(1, "openai", "replacement", 3, 1, true) // idempotent retry
}

func TestUserPlatformConcurrencyTracksUnlimitedAndUpdates(t *testing.T) {
	c, _ := platformTestCache(t)
	ctx := context.Background()
	for _, id := range []string{"one", "two"} {
		ok, err := c.AcquireUserPlatformSlot(ctx, 1, "openai", 0, 0, id)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := c.AcquireUserPlatformSlot(ctx, 1, "openai", 0, 1, "three")
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, c.ReleaseUserPlatformSlot(ctx, 1, "openai", "one"))
	ok, err = c.AcquireUserPlatformSlot(ctx, 1, "openai", 0, 1, "three")
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, c.ReleaseUserPlatformSlot(ctx, 1, "openai", "two"))
	ok, err = c.AcquireUserPlatformSlot(ctx, 1, "openai", 0, 1, "three")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = c.AcquireUserPlatformSlot(ctx, 1, "openai", 0, 0, "four")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = c.AcquireUserPlatformSlot(ctx, 1, "openai", 0, 1, "five")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestUserPlatformConcurrencyContenders(t *testing.T) {
	c, _ := platformTestCache(t)
	ctx := context.Background()
	var acquired atomic.Int64
	var wg sync.WaitGroup
	errors := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := c.AcquireUserPlatformSlot(ctx, 7, "openai", 10, 2, fmt.Sprint(i))
			if err != nil {
				errors <- err
			}
			if ok {
				acquired.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, acquired.Load())
	count, err := c.GetUserConcurrency(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestUserPlatformConcurrencyExpires(t *testing.T) {
	c, server := platformTestCache(t)
	ctx := context.Background()
	ok, err := c.AcquireUserPlatformSlot(ctx, 7, "openai", 1, 1, "expired")
	require.NoError(t, err)
	require.True(t, ok)
	server.FastForward(16 * time.Minute)
	ok, err = c.AcquireUserPlatformSlot(ctx, 7, "openai", 1, 1, "new")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestUserPlatformConcurrencyLiveTransfer(t *testing.T) {
	for _, total := range []int{0, 3} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			c, _ := platformTestCache(t)
			ctx := context.Background()
			ok, err := c.AcquireUserPlatformSlot(ctx, 20, "openai", total, 1, "source")
			require.NoError(t, err)
			require.True(t, ok)
			request := service.LiveLeaseTransferRequest{AccountID: 10, UserID: 20, APIKeyID: 30, UserMax: total, UserPlatformMax: 1, UserRequestID: "source", LeaseID: "live"}
			ok, err = c.AcquireLiveLeaseTransferring(ctx, request)
			require.NoError(t, err)
			require.True(t, ok)
			require.Zero(t, c.rdb.ZCard(ctx, userPlatformSlotKey(20, "openai")).Val())
			count, err := c.GetUserConcurrency(ctx, 20)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			require.NoError(t, c.ReleaseUserPlatformSlot(ctx, 20, "openai", "source")) // stale HTTP cleanup
			ok, err = c.AcquireUserPlatformSlot(ctx, 20, "openai", total, 1, "blocked")
			require.NoError(t, err)
			require.False(t, ok)
			ok, err = c.AcquireUserPlatformSlot(ctx, 20, "anthropic", total, 1, "other")
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = c.RefreshLiveLease(ctx, 10, 20, 30, "live")
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, c.ReleaseLiveLease(ctx, 10, 20, 30, "live"))
			ok, err = c.AcquireUserPlatformSlot(ctx, 20, "openai", total, 1, "after")
			require.NoError(t, err)
			require.True(t, ok)
		})
	}
}

func TestUserPlatformConcurrencyLiveTransferKeepsSharedTTL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		apiKeyTTL   int
		sourceAge   time.Duration
		otherAge    time.Duration
		other       bool
		wantAllowed bool
	}{
		{name: "short key TTL preserves platform source", apiKeyTTL: 30, sourceAge: time.Minute, wantAllowed: true},
		{name: "short key TTL preserves competing platform slot", apiKeyTTL: 30, other: true, otherAge: time.Minute},
		{name: "long key TTL does not extend platform slot", apiKeyTTL: 1800, other: true, otherAge: 16 * time.Minute, wantAllowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newDedicatedAPIKeyTTLCache(t, tc.apiKeyTTL)
			ctx := context.Background()
			now, err := c.redisUnixSeconds(ctx)
			require.NoError(t, err)
			platformKey := userPlatformSlotKey(20, "openai")
			require.NoError(t, c.rdb.ZAdd(ctx, platformKey, redis.Z{Score: float64(now) - tc.sourceAge.Seconds(), Member: "source"}).Err())
			if tc.other {
				require.NoError(t, c.rdb.ZAdd(ctx, platformKey, redis.Z{Score: float64(now) - tc.otherAge.Seconds(), Member: "other"}).Err())
			}
			ok, err := c.AcquireLiveLeaseTransferring(ctx, service.LiveLeaseTransferRequest{
				AccountID: 10, UserID: 20, APIKeyID: 30,
				UserPlatformMax: 1, UserRequestID: "source", LeaseID: "live",
			})
			require.Equal(t, tc.wantAllowed, ok)
			if tc.wantAllowed {
				require.NoError(t, err)
				require.NoError(t, c.rdb.ZScore(ctx, liveUserSlotKey(20), "live").Err())
				require.Zero(t, c.rdb.ZCard(ctx, platformKey).Val())
			} else {
				require.ErrorIs(t, err, service.ErrLiveConcurrencyFull)
				require.EqualValues(t, 2, c.rdb.ZCard(ctx, platformKey).Val())
				require.Zero(t, c.rdb.ZCard(ctx, liveUserSlotKey(20)).Val())
			}
		})
	}
}

func TestUserPlatformConcurrencyLiveAbortAndLostSource(t *testing.T) {
	c, _ := platformTestCache(t)
	ctx := context.Background()
	request := service.LiveLeaseTransferRequest{AccountID: 10, UserID: 20, APIKeyID: 30, UserMax: 0, UserPlatformMax: 1, UserRequestID: "source", LeaseID: "live"}
	ok, err := c.AcquireLiveLeaseTransferring(ctx, request)
	require.ErrorIs(t, err, service.ErrLiveLeaseSourceLost)
	require.False(t, ok)
	ok, err = c.AcquireUserPlatformSlot(ctx, 20, "openai", 0, 1, "source")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, c.AbortLiveLeaseTransfer(ctx, request))
	require.Zero(t, c.rdb.ZCard(ctx, userPlatformSlotKey(20, "openai")).Val())
	ok, err = c.AcquireLiveLeaseTransferring(ctx, request)
	require.ErrorIs(t, err, service.ErrLiveLeaseTransferFenced)
	require.False(t, ok)
}
