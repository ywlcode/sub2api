//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type penaltyConfigSettings struct {
	SettingRepository
	read func(context.Context) (map[string]string, error)
}

func (r penaltyConfigSettings) GetMultiple(ctx context.Context, _ []string) (map[string]string, error) {
	return r.read(ctx)
}

func penaltyConfigValues(t *testing.T, cfg SecurityPenaltyConfig) map[string]string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	return map[string]string{SettingKeySecurityPenalty: string(raw), SettingKeyRiskControlEnabled: "true", SettingKeyCyberPolicyUserAllowlist: "12"}
}

func connectedPenaltyConfig(t *testing.T, repo SettingRepository) (*SecurityPenaltyService, *APIKeyService) {
	t.Helper()
	keys := &APIKeyService{cache: &struct{ APIKeyCache }{}}
	keys.authInvalidationConnected.Store(true)
	return NewSecurityPenaltyService(nil, repo, keys), keys
}

func TestPenaltyConfigCacheRetainsDisabledConfigUntilInvalidationAndOwnsValues(t *testing.T) {
	cfg := DefaultSecurityPenaltyConfig()
	id := int64(2)
	cfg.TargetGroupID, cfg.GroupIDs, cfg.ExemptUserIDs = &id, []int64{2}, []int64{7}
	values := penaltyConfigValues(t, cfg)
	calls := 0
	s, keys := connectedPenaltyConfig(t, penaltyConfigSettings{read: func(context.Context) (map[string]string, error) {
		calls++
		return values, nil
	}})
	for range 3 {
		got, err := s.GetConfig(context.Background())
		require.NoError(t, err)
		require.False(t, got.Enabled)
		require.Equal(t, int64(2), *got.TargetGroupID)
		require.Equal(t, []int64{2}, got.GroupIDs)
		require.Equal(t, []int64{7}, got.ExemptUserIDs)
		require.Equal(t, []int64{12}, got.trustedUserIDs)
		*got.TargetGroupID, got.GroupIDs[0], got.ExemptUserIDs[0], got.trustedUserIDs[0] = 999, 999, 999, 999
	}
	require.Equal(t, 1, calls)
	require.True(t, s.configCache.entries[0].expires.IsZero(), "healthy event-driven cache has no periodic expiry")
	cfg.Enabled = true
	values = penaltyConfigValues(t, cfg)
	keys.invalidateLocalAuthCache(securityPenaltyConfigInvalidationKey)
	got, err := s.GetConfig(context.Background())
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, 2, calls)
	keys.authInvalidationConnected.Store(false)
	values[SettingKeyRiskControlEnabled] = "false"
	for range 2 {
		got, err = s.GetConfig(context.Background())
		require.NoError(t, err)
		require.False(t, got.RiskControlEnabled)
	}
	require.Equal(t, 4, calls, "offline invalidation must bypass old grants")
	keys.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "all:0")
	keys.authInvalidationConnected.Store(true)
	got, err = s.GetConfig(context.Background())
	require.NoError(t, err)
	require.False(t, got.RiskControlEnabled)
	require.Equal(t, 5, calls, "reconnection clears potentially missed changes")
}

func TestPenaltyConfigCacheCoalescesConcurrentReads(t *testing.T) {
	values := penaltyConfigValues(t, DefaultSecurityPenaltyConfig())
	var calls atomic.Int32
	s, _ := connectedPenaltyConfig(t, penaltyConfigSettings{read: func(context.Context) (map[string]string, error) {
		calls.Add(1)
		return values, nil
	}})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.GetConfig(context.Background())
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), calls.Load(), "100 concurrent requests share one settings query")
}

func TestPenaltyConfigCacheCanceledFollowerAndInvalidatedLoader(t *testing.T) {
	old := penaltyConfigValues(t, DefaultSecurityPenaltyConfig())
	newConfig := DefaultSecurityPenaltyConfig()
	newConfig.Enabled = true
	updated := penaltyConfigValues(t, newConfig)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, keys := connectedPenaltyConfig(t, penaltyConfigSettings{read: func(context.Context) (map[string]string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return old, nil
		}
		return updated, nil
	}})
	done := make(chan SecurityPenaltyConfig, 1)
	go func() { value, _ := s.GetConfig(context.Background()); done <- value }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	follower := make(chan error, 1)
	go func() { _, err := s.GetConfig(ctx); follower <- err }()
	cancel()
	select {
	case err := <-follower:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled request waited for the settings query")
	}
	// A concurrent save/reconnect must not wait for a blocked settings query,
	// and its earlier loader must not repopulate or return the old grant.
	keys.invalidateLocalAuthCache(securityPenaltyConfigInvalidationKey)
	got, err := s.GetConfig(context.Background())
	require.NoError(t, err)
	require.True(t, got.Enabled)
	close(release)
	require.True(t, (<-done).Enabled)
	require.Equal(t, int32(2), calls.Load())
}

func TestPenaltyConfigCacheCanceledLeaderDoesNotCancelSharedRefresh(t *testing.T) {
	values := penaltyConfigValues(t, DefaultSecurityPenaltyConfig())
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, _ := connectedPenaltyConfig(t, penaltyConfigSettings{read: func(ctx context.Context) (map[string]string, error) {
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return values, nil
		}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { _, err := s.GetConfig(ctx); leader <- err }()
	<-started
	cancel()
	select {
	case err := <-leader:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not leave")
	}
	close(release)
	_, err := s.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
}

func TestPenaltyConfigCacheErrorsDoNotReuseOldPermissions(t *testing.T) {
	values := penaltyConfigValues(t, DefaultSecurityPenaltyConfig())
	var loadErr error
	calls := 0
	s, keys := connectedPenaltyConfig(t, penaltyConfigSettings{read: func(context.Context) (map[string]string, error) {
		calls++
		return values, loadErr
	}})
	_, err := s.GetConfig(context.Background())
	require.NoError(t, err)
	keys.invalidateLocalAuthCache(securityPenaltyConfigInvalidationKey)
	loadErr = errors.New("database unavailable")
	for range 2 {
		_, err = s.GetConfig(context.Background())
		require.ErrorIs(t, err, loadErr)
	}
	require.Equal(t, 3, calls)
	loadErr = nil
	values[SettingKeyCyberPolicyUserAllowlist] = "invalid"
	_, err = s.GetConfig(context.Background())
	require.Error(t, err)
}

func TestPenaltyConfigCacheInvalidationAcrossReplicas(t *testing.T) {
	bus := &configNotificationBus{}
	values := penaltyConfigValues(t, DefaultSecurityPenaltyConfig())
	repo := penaltyConfigSettings{read: func(context.Context) (map[string]string, error) { return values, nil }}
	a, b := &APIKeyService{cache: bus}, &APIKeyService{cache: bus}
	first, second := NewSecurityPenaltyService(nil, repo, a), NewSecurityPenaltyService(nil, repo, b)
	a.StartAuthCacheInvalidationSubscriber(context.Background())
	b.StartAuthCacheInvalidationSubscriber(context.Background())
	t.Cleanup(a.StopAuthCacheInvalidationSubscriber)
	t.Cleanup(b.StopAuthCacheInvalidationSubscriber)
	require.Eventually(t, func() bool { return a.authInvalidationConnected.Load() && b.authInvalidationConnected.Load() }, time.Second, time.Millisecond)
	for _, svc := range []*SecurityPenaltyService{first, second} {
		_, err := svc.GetConfig(context.Background())
		require.NoError(t, err)
	}
	values[SettingKeyCyberPolicyUserAllowlist] = "34"
	first.invalidateConfig()
	for _, svc := range []*SecurityPenaltyService{first, second} {
		got, err := svc.GetConfig(context.Background())
		require.NoError(t, err)
		require.Equal(t, []int64{34}, got.trustedUserIDs)
	}
}

func TestPenaltyConfigSaveInvalidatesOnlyAfterSuccessfulCommit(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "version conflict"}[conflict], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			cfg := DefaultSecurityPenaltyConfig()
			values := penaltyConfigValues(t, cfg)
			oldJSON := values[SettingKeySecurityPenalty]
			reads := 0
			keys := &APIKeyService{cache: &configNotificationBus{}}
			keys.authInvalidationConnected.Store(true)
			s := NewSecurityPenaltyService(db, penaltyConfigSettings{read: func(context.Context) (map[string]string, error) {
				reads++
				return values, nil
			}}, keys)
			_, err = s.GetConfig(context.Background())
			require.NoError(t, err)
			cfg.Enabled = true
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT value FROM settings .*FOR UPDATE").WithArgs(SettingKeySecurityPenalty).
				WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(oldJSON))
			if conflict {
				cfg.Version++
				mock.ExpectRollback()
				_, err = s.SaveConfig(context.Background(), cfg)
				require.Error(t, err)
				got, err := s.GetConfig(context.Background())
				require.NoError(t, err)
				require.False(t, got.Enabled)
				require.Equal(t, 1, reads, "rolled-back writes leave a valid snapshot intact")
			} else {
				updated := cfg
				updated.Version++
				values = penaltyConfigValues(t, updated)
				mock.ExpectExec("UPDATE settings SET value").WithArgs(SettingKeySecurityPenalty, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
				got, err := s.SaveConfig(context.Background(), cfg)
				require.NoError(t, err)
				require.True(t, got.Enabled)
				require.Equal(t, int64(2), got.Version)
				require.Equal(t, 2, reads, "committed save must reload the configuration")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
