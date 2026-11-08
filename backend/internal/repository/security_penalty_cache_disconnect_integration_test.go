//go:build integration

package repository

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestPenaltyCacheSubscriberDisconnectAndRecovery(t *testing.T) {
	ctx := context.Background()
	settings := NewSettingRepository(integrationEntClient)
	old, err := settings.GetValue(ctx, service.SettingKeyRiskControlEnabled)
	if err == service.ErrSettingNotFound {
		old, err = "", nil
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, settings.Set(ctx, service.SettingKeyRiskControlEnabled, old)) })
	require.NoError(t, settings.Set(ctx, service.SettingKeyRiskControlEnabled, "false"))
	var offline atomic.Bool
	var mu sync.Mutex
	var conns []net.Conn
	options := *integrationRedis.Options()
	options.DialTimeout = 100 * time.Millisecond
	options.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if offline.Load() {
			return nil, fmt.Errorf("simulated subscription-only network partition")
		}
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		if err == nil {
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
		return conn, err
	}
	subscriberRedis := redis.NewClient(&options)
	t.Cleanup(func() { _ = subscriberRedis.Close() })
	keys := service.NewAPIKeyService(nil, nil, nil, nil, nil, NewAPIKeyCache(subscriberRedis), &config.Config{})
	penalty := service.NewSecurityPenaltyService(integrationDB, settings, keys)
	keys.StartAuthCacheInvalidationSubscriber(ctx)
	t.Cleanup(keys.StopAuthCacheInvalidationSubscriber)
	require.Eventually(t, func() bool { return keys.AuthCacheInvalidationSubscriberHealth().Connected }, 2*time.Second, 10*time.Millisecond)
	cfg, err := penalty.GetConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.RiskControlEnabled)
	offline.Store(true)
	mu.Lock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	mu.Unlock()
	require.NoError(t, settings.Set(ctx, service.SettingKeyRiskControlEnabled, "true"))
	require.NoError(t, NewAPIKeyCache(integrationRedis).PublishAuthCacheInvalidation(ctx, penaltyConfigEventKey()))
	require.Eventually(t, func() bool { return !keys.AuthCacheInvalidationSubscriberHealth().Connected }, 2*time.Second, 10*time.Millisecond)
	cfg, err = penalty.GetConfig(ctx)
	require.NoError(t, err)
	require.True(t, cfg.RiskControlEnabled, "offline subscriber must bypass stale configuration")
	// Updates during an outage may be published successfully with no listener.
	// A confirmed reconnect must discard all pre-partition snapshots.
	offline.Store(false)
	require.Eventually(t, func() bool { return keys.AuthCacheInvalidationSubscriberHealth().Connected }, 5*time.Second, 10*time.Millisecond)
	cfg, err = penalty.GetConfig(ctx)
	require.NoError(t, err)
	require.True(t, cfg.RiskControlEnabled)
	require.NoError(t, settings.Set(ctx, service.SettingKeyRiskControlEnabled, "false"))
	require.NoError(t, NewAPIKeyCache(integrationRedis).PublishAuthCacheInvalidation(ctx, penaltyConfigEventKey()))
	require.Eventually(t, func() bool {
		cfg, err := penalty.GetConfig(ctx)
		return err == nil && !cfg.RiskControlEnabled
	}, time.Second, 10*time.Millisecond, "recovered subscriber must deliver future invalidations")
}
