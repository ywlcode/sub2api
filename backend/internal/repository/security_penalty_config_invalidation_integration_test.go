//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func penaltyConfigEventKey() string {
	sum := sha256.Sum256([]byte("sub2api:security-penalty-config:v1"))
	return hex.EncodeToString(sum[:])
}

func TestPenaltyConfigTriggerCoversAllSettingsAndTransactionalChanges(t *testing.T) {
	ctx := context.Background()
	for _, key := range []string{service.SettingKeySecurityPenalty, service.SettingKeyRiskControlEnabled, service.SettingKeyCyberPolicyUserAllowlist} {
		t.Run(key, func(t *testing.T) {
			tx, err := integrationDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key=$1", penaltyConfigEventKey())
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", key)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key=$1", penaltyConfigEventKey())
			require.NoError(t, err)
			count := func() int {
				var n int
				require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM auth_cache_invalidation_outbox WHERE cache_key=$1", penaltyConfigEventKey()).Scan(&n))
				return n
			}
			_, err = tx.ExecContext(ctx, "SAVEPOINT config_mutation")
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'before')", key)
			require.NoError(t, err)
			require.Equal(t, 1, count(), "first-time configuration is an invalidation")
			_, err = tx.ExecContext(ctx, "UPDATE settings SET value=value,updated_at=NOW() WHERE key=$1", key)
			require.NoError(t, err)
			require.Equal(t, 1, count(), "unchanged saves do not enqueue")
			_, err = tx.ExecContext(ctx, "UPDATE settings SET value='after' WHERE key=$1", key)
			require.NoError(t, err)
			require.Equal(t, 2, count())
			_, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", key)
			require.NoError(t, err)
			require.Equal(t, 3, count(), "deletion must revoke an old cached setting")
			_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT config_mutation")
			require.NoError(t, err)
			require.Zero(t, count(), "a rolled-back write must not emit a durable event")
			_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('penalty-test-unrelated','value')")
			require.NoError(t, err)
			require.Zero(t, count())
		})
	}
}

func TestPenaltyConfigDirectSQLChangeRefreshesTwoReplicasThroughDurableOutbox(t *testing.T) {
	ctx := context.Background()
	repo := NewSettingRepository(integrationEntClient)
	old, err := repo.GetValue(ctx, service.SettingKeyCyberPolicyUserAllowlist)
	if err == service.ErrSettingNotFound {
		old, err = "", nil
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Set(ctx, service.SettingKeyCyberPolicyUserAllowlist, old)) })
	require.NoError(t, repo.Set(ctx, service.SettingKeyCyberPolicyUserAllowlist, ""))
	oldRisk, err := repo.GetValue(ctx, service.SettingKeyRiskControlEnabled)
	if err == service.ErrSettingNotFound {
		oldRisk, err = "", nil
	}
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Set(ctx, service.SettingKeyRiskControlEnabled, oldRisk)) })
	require.NoError(t, repo.Set(ctx, service.SettingKeyRiskControlEnabled, "false"))
	cache := NewAPIKeyCache(integrationRedis)
	keys := []*service.APIKeyService{
		service.NewAPIKeyService(nil, nil, nil, nil, nil, cache, &config.Config{}),
		service.NewAPIKeyService(nil, nil, nil, nil, nil, cache, &config.Config{}),
	}
	penalties := make([]*service.SecurityPenaltyService, 0, 2)
	for _, key := range keys {
		penalties = append(penalties, service.NewSecurityPenaltyService(integrationDB, repo, key))
		key.StartAuthCacheInvalidationSubscriber(ctx)
		t.Cleanup(key.StopAuthCacheInvalidationSubscriber)
	}
	require.Eventually(t, func() bool {
		return keys[0].AuthCacheInvalidationSubscriberHealth().Connected && keys[1].AuthCacheInvalidationSubscriberHealth().Connected
	}, 3*time.Second, 10*time.Millisecond)
	for _, svc := range penalties {
		cfg, err := svc.GetConfig(ctx)
		require.NoError(t, err)
		require.False(t, cfg.RiskControlEnabled)
	}
	// No service-layer callback: the database trigger alone must notify peers.
	require.NoError(t, repo.Set(ctx, service.SettingKeyRiskControlEnabled, "true"))
	worker := service.NewAuthCacheInvalidationWorker(NewAuthCacheInvalidationOutboxRepository(integrationDB), cache, keys[0])
	worker.Start()
	t.Cleanup(worker.Stop)
	require.Eventually(t, func() bool {
		for _, svc := range penalties {
			cfg, err := svc.GetConfig(ctx)
			if err != nil || !cfg.RiskControlEnabled {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "durable delivery must invalidate both process caches")
}
