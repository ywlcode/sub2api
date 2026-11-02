package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newAPIKeyHealthTestCache(t *testing.T) (*miniredis.Miniredis, service.APIKeyHealthCache, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	server.SetTime(time.Unix(1_700_000_000, 0))
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, NewTempUnschedCache(client).(service.APIKeyHealthCache), client
}

func TestAPIKeyHealthCachePhysicalAttemptsAndSlidingEviction(t *testing.T) {
	_, cache, client := newAPIKeyHealthTestCache(t)
	ctx := context.Background()
	config := service.APIKeyHealthConfig{Enabled: true, WindowSize: 3, FailureThreshold: 2}
	start, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	// The first failure is evicted by successful physical calls.
	for _, success := range []bool{false, true, true, true, false} {
		trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, success, 502, "upstream error")
		require.NoError(t, err)
		require.Nil(t, trip)
	}
	values, err := client.LRange(ctx, apiKeyHealthKeys(42)[1], 0, -1).Result()
	require.NoError(t, err)
	require.Equal(t, []string{"1", "1", "0"}, values)
	trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 400, "upstream error")
	require.NoError(t, err)
	require.True(t, trip.Tripped)
	require.Equal(t, 2, trip.Failures)
	require.Equal(t, 3, trip.Samples)
	require.Equal(t, int64(900), trip.UntilUnix-trip.TriggeredAtUnix)
	require.EqualValues(t, 0, client.LLen(ctx, apiKeyHealthKeys(42)[1]).Val())
}

func TestAPIKeyHealthCacheDefaultTripsBeforeWindowIsFull(t *testing.T) {
	_, cache, _ := newAPIKeyHealthTestCache(t)
	ctx := context.Background()
	config := service.DefaultAPIKeyHealthConfig()
	start, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	for i := 1; i <= 10; i++ {
		trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 500, "")
		require.NoError(t, err)
		if i < 10 {
			require.Nil(t, trip)
		} else {
			require.Equal(t, 100, trip.WindowSize)
			require.Equal(t, 10, trip.Samples)
			require.Equal(t, 1, trip.TripCount)
		}
	}
}

func TestAPIKeyHealthCacheEscalationAndLateResults(t *testing.T) {
	server, cache, _ := newAPIKeyHealthTestCache(t)
	ctx := context.Background()
	config := service.APIKeyHealthConfig{Enabled: true, WindowSize: 100, FailureThreshold: 1}
	for index, seconds := range []int64{900, 1800, 3600, 3600} {
		start, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
		require.NoError(t, err)
		require.Zero(t, start.UntilUnix)
		trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 429, "limited")
		require.NoError(t, err)
		require.Equal(t, index+1, trip.TripCount)
		require.Equal(t, seconds, trip.UntilUnix-trip.TriggeredAtUnix)
		blocked, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
		require.NoError(t, err)
		require.Equal(t, trip.UntilUnix, blocked.UntilUnix)
		late, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 502, "late")
		require.NoError(t, err)
		require.Nil(t, late)
		server.SetTime(time.Unix(trip.UntilUnix+1, 0))
		// Old in-flight completions remain fenced even after the cooldown ends.
		late, err = cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, true, 200, "")
		require.NoError(t, err)
		require.Nil(t, late)
	}
}

func TestAPIKeyHealthCacheConcurrentFailuresTripOnlyOnce(t *testing.T) {
	_, cache, _ := newAPIKeyHealthTestCache(t)
	ctx := context.Background()
	config := service.APIKeyHealthConfig{Enabled: true, WindowSize: 100, FailureThreshold: 1}
	start, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	var trips atomic.Int64
	var failures atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 40; i++ {
		group.Go(func() {
			trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 502, "")
			if err != nil {
				failures.Add(1)
			} else if trip != nil && trip.Tripped {
				trips.Add(1)
			}
		})
	}
	group.Wait()
	require.Zero(t, failures.Load())
	require.EqualValues(t, 1, trips.Load())
	state, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	require.Equal(t, 1, state.TripCount)
}

func TestAPIKeyHealthCacheSixHourResetRequiresSuccess(t *testing.T) {
	for _, successAt := range []time.Duration{-1, 2 * time.Hour, 6 * time.Hour} {
		t.Run(successAt.String(), func(t *testing.T) {
			server, cache, _ := newAPIKeyHealthTestCache(t)
			ctx := context.Background()
			config := service.APIKeyHealthConfig{Enabled: true, WindowSize: 100, FailureThreshold: 1}
			start, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
			require.NoError(t, err)
			first, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 500, "")
			require.NoError(t, err)
			base := time.Unix(first.TriggeredAtUnix, 0)
			if successAt >= 0 {
				server.SetTime(base.Add(successAt))
				start, err = cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
				require.NoError(t, err)
				_, err = cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, true, 200, "")
				require.NoError(t, err)
				if successAt < 6*time.Hour {
					state, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
					require.NoError(t, err)
					require.Equal(t, 1, state.TripCount, "one success must not reset escalation early")
				}
			}
			server.SetTime(base.Add(6 * time.Hour))
			start, err = cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
			require.NoError(t, err)
			trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, start.Generation, false, 500, "")
			require.NoError(t, err)
			if successAt < 0 {
				require.Equal(t, 2, trip.TripCount)
				require.EqualValues(t, 1800, trip.UntilUnix-trip.TriggeredAtUnix)
			} else {
				require.Equal(t, 1, trip.TripCount)
				require.EqualValues(t, 900, trip.UntilUnix-trip.TriggeredAtUnix)
			}
		})
	}
}

func TestAPIKeyHealthCacheConfigurationAndManualReset(t *testing.T) {
	_, cache, _ := newAPIKeyHealthTestCache(t)
	ctx := context.Background()
	config := service.APIKeyHealthConfig{Enabled: true, WindowSize: 100, FailureThreshold: 2}
	old, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	_, err = cache.RecordAPIKeyHealthResult(ctx, 42, old.Generation, false, 500, "")
	require.NoError(t, err)
	config.WindowSize = 20
	current, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	require.Greater(t, current.Generation, old.Generation)
	trip, err := cache.RecordAPIKeyHealthResult(ctx, 42, old.Generation, false, 500, "")
	require.NoError(t, err)
	require.Nil(t, trip)
	trip, err = cache.RecordAPIKeyHealthResult(ctx, 42, current.Generation, false, 500, "")
	require.NoError(t, err)
	require.Nil(t, trip, "configuration edits must clear the old window")
	trip, err = cache.RecordAPIKeyHealthResult(ctx, 42, current.Generation, false, 500, "")
	require.NoError(t, err)
	require.Equal(t, 1, trip.TripCount)
	require.NoError(t, cache.ResetAPIKeyHealth(ctx, 42))
	reset, err := cache.BeginAPIKeyHealthAttempt(ctx, 42, config)
	require.NoError(t, err)
	require.Zero(t, reset.UntilUnix)
	require.Zero(t, reset.TripCount)
	require.Greater(t, reset.Generation, trip.Generation)
	other, err := cache.BeginAPIKeyHealthAttempt(ctx, 43, config)
	require.NoError(t, err)
	require.Zero(t, other.TripCount)
}
