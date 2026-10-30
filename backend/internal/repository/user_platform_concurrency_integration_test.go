//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func (s *APIKeyRepoSuite) TestPlatformConcurrencyAuthProjection() {
	user := s.mustCreateUser("platform-auth@test.com")
	_, err := s.client.User.UpdateOneID(user.ID).SetPlatformConcurrencyLimits(map[string]int{service.PlatformOpenAI: 2, service.PlatformGemini: 3}).Save(s.ctx)
	s.Require().NoError(err)
	key := &service.APIKey{UserID: user.ID, Key: "platform-auth-test-key", Name: "test", Status: service.StatusActive}
	s.Require().NoError(s.repo.Create(s.ctx, key))
	for _, lookup := range []func(context.Context, string) (*service.APIKey, error){s.repo.GetByKey, s.repo.GetByKeyForAuth} {
		got, err := lookup(s.ctx, key.Key)
		s.Require().NoError(err)
		s.Require().Equal(map[string]int{service.PlatformOpenAI: 2, service.PlatformGemini: 3}, got.User.PlatformConcurrencyLimits)
	}
}

func (s *UserRepoSuite) TestPlatformConcurrencyRoundTripAndPartialUpdate() {
	user := s.mustCreateUser(&service.User{PlatformConcurrencyLimits: map[string]int{"openai": 2}})
	got, err := s.repo.GetByID(s.ctx, user.ID)
	s.Require().NoError(err)
	s.Require().Equal(map[string]int{"openai": 2}, got.PlatformConcurrencyLimits)
	stale := *got
	got.PlatformConcurrencyLimits = map[string]int{"gemini": 3}
	s.Require().NoError(s.repo.Update(s.ctx, got, service.UserUpdateFields{PlatformConcurrencyLimits: true}))
	stale.Username = "profile-update"
	s.Require().NoError(s.repo.Update(s.ctx, &stale, service.UserUpdateFields{Username: true}))
	got, err = s.repo.GetByID(s.ctx, user.ID)
	s.Require().NoError(err)
	s.Require().Equal(map[string]int{"gemini": 3}, got.PlatformConcurrencyLimits)
	s.Require().ErrorContains(checkRegionalPlatformConcurrency(integrationDB), "does not support")
	got.PlatformConcurrencyLimits = map[string]int{}
	s.Require().NoError(s.repo.Update(s.ctx, got, service.UserUpdateFields{PlatformConcurrencyLimits: true}))
	got, err = s.repo.GetByID(s.ctx, user.ID)
	s.Require().NoError(err)
	s.Require().Empty(got.PlatformConcurrencyLimits)
	s.Require().NoError(checkRegionalPlatformConcurrency(integrationDB))
}

func BenchmarkUserPlatformConcurrencyAdmission(b *testing.B) {
	for _, platform := range []bool{false, true} {
		name := "total"
		if platform {
			name = "total_and_platform"
		}
		b.Run(name, func(b *testing.B) {
			client := redis.NewClient(integrationRedis.Options())
			defer client.Close()
			prefix := fmt.Sprintf("platform-bench:%d:", time.Now().UnixNano())
			client.AddHook(prefixHook{prefix: prefix})
			cache := NewConcurrencyCache(client, 15, 900).(*concurrencyCache)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var ok bool
				var err error
				if platform {
					ok, err = cache.AcquireUserPlatformSlot(ctx, 1, "openai", 10, 2, "request")
				} else {
					ok, err = cache.AcquireUserSlot(ctx, 1, 10, "request")
				}
				if err != nil || !ok {
					b.Fatalf("acquire: %t %v", ok, err)
				}
				if platform {
					err = cache.ReleaseUserPlatformSlot(ctx, 1, "openai", "request")
				} else {
					err = cache.ReleaseUserSlot(ctx, 1, "request")
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := integrationRedis.Del(ctx, prefix+userActiveIndexKey).Err(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func TestUserPlatformConcurrencyMigrationInvalidatesAllKeys(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	var id int64
	var limits string
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO users(email,password_hash) VALUES ('platform-concurrency@test.com','test') RETURNING id,platform_concurrency_limits::text`).Scan(&id, &limits))
	require.Equal(t, "{}", limits)
	for i := 0; i < 2; i++ {
		_, err := tx.ExecContext(ctx, `INSERT INTO api_keys(user_id,key,name) VALUES ($1,$2,'test')`, id, fmt.Sprintf("platform-concurrency-key-%d", i))
		require.NoError(t, err)
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM auth_cache_invalidation_outbox`)
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]int{"openai": 2})
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE users SET platform_concurrency_limits=$2::jsonb WHERE id=$1`, id, string(payload))
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM auth_cache_invalidation_outbox`).Scan(&count))
	require.Equal(t, 2, count)
	_, err = tx.ExecContext(ctx, `UPDATE users SET platform_concurrency_limits=$2::jsonb WHERE id=$1`, id, string(payload))
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM auth_cache_invalidation_outbox`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestUserPlatformConcurrencyIndependentRedisClients(t *testing.T) {
	prefix := fmt.Sprintf("platform-race:%d:", time.Now().UnixNano())
	clients := []*redis.Client{redis.NewClient(integrationRedis.Options()), redis.NewClient(integrationRedis.Options())}
	caches := make([]*concurrencyCache, 2)
	for i, client := range clients {
		client.AddHook(prefixHook{prefix: prefix})
		caches[i] = NewConcurrencyCache(client, 15, 900).(*concurrencyCache)
	}
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	var admitted atomic.Int64
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := caches[i%2].AcquireUserPlatformSlot(ctx, 991, "openai", 10, 2, fmt.Sprint(i))
			if err != nil {
				errs <- err
			}
			if ok {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, admitted.Load())
	count, err := caches[0].GetUserConcurrency(ctx, 991)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	for i := 0; i < 64; i++ {
		require.NoError(t, caches[i%2].ReleaseUserPlatformSlot(ctx, 991, "openai", fmt.Sprint(i)))
	}
	count, err = caches[1].GetUserConcurrency(ctx, 991)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, integrationRedis.Del(ctx, prefix+userActiveIndexKey).Err())
}
