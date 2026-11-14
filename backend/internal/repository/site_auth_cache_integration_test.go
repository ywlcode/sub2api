//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteAuthCacheTracksConfigCommitsNotMoneyOrHeartbeats(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	var credential string
	require.NoError(t, p.db.QueryRow(`SELECT key FROM api_keys WHERE id=$1`, p.key).Scan(&credential))
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary, ID: p.peer.ID, Token: p.peer.Token}}
	svc := service.NewAPIKeyService(NewAPIKeyRepository(nil, p.replica), nil, nil, nil, nil, nil, cfg)
	starts, finishes := 0, 0
	p.local.BeforeAuthCommit = func() func() {
		starts++
		finish := svc.BeginSiteAuthInstall()
		return func() { finish(); finishes++ }
	}
	p.local.AfterInstall = func(context.Context) error { svc.InvalidateSiteReplica(); return nil }
	get := func() *service.APIKey {
		t.Helper()
		key, err := svc.GetByKey(ctx, credential)
		require.NoError(t, err)
		return key
	}
	require.Equal(t, float64(100), get().User.Balance)
	get()
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().SecondaryConfigLoads)
	p.sync(t)
	get()
	require.Zero(t, starts, "a heartbeat must not invalidate auth configuration")
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().SecondaryConfigLoads)
	_, err := p.db.Exec(`UPDATE users SET balance=90 WHERE id=$1`, p.user)
	require.NoError(t, err)
	p.sync(t)
	require.Equal(t, float64(90), get().User.Balance)
	require.Zero(t, starts, "a monetary diff must keep identity/permission cache hot")
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().SecondaryConfigLoads)
	p.bill(t)
	require.Equal(t, float64(89), get().User.Balance, "local unreported consumption must be seen on a cache hit")
	p.sync(t)
	require.Equal(t, float64(89), get().User.Balance, "pending overlay must not be lost or doubled")
	require.Zero(t, starts)
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().SecondaryConfigLoads)
	_, err = p.db.Exec(`UPDATE api_keys SET model_mapping='{"alias":"new-model"}' WHERE id=$1`, p.key)
	require.NoError(t, err)
	p.sync(t)
	require.Equal(t, 1, starts)
	require.Equal(t, starts, finishes)
	require.Equal(t, "new-model", get().ModelMapping["alias"])
	require.Equal(t, uint64(2), svc.AuthLookupMetrics().SecondaryConfigLoads)
	_, err = p.db.Exec(`INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier,denied_models)
		SELECT user_id,group_id,1,'["newly-denied"]' FROM api_keys WHERE id=$1`, p.key)
	require.NoError(t, err)
	p.sync(t)
	require.Equal(t, []string{"newly-denied"}, get().User.UserGroupDeniedModels)
	require.Equal(t, starts, finishes)
	// Even a post-commit policy failure must close the bracket and discard old
	// config. The HTTP site gate independently withholds lease renewal on error.
	p.local.AfterInstall = func(context.Context) error { return errors.New("injected policy failure") }
	_, err = p.db.Exec(`UPDATE api_keys SET model_mapping='{"alias":"after-error"}' WHERE id=$1`, p.key)
	require.NoError(t, err)
	snap, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: p.version})
	require.NoError(t, err)
	err = p.local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: snap.Full, BaseVersion: p.version, Version: snap.Version, Rows: snap.Rows})
	require.ErrorContains(t, err, "injected policy failure")
	require.Equal(t, starts, finishes)
	require.Equal(t, "after-error", get().ModelMapping["alias"])
	get()
	require.Equal(t, 1, svc.AuthLookupMetrics().SecondaryCacheEntries, "error cleanup must not leave cache permanently bypassed")
}

func TestSiteAuthCacheRecoversAfterRolledBackInstall(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	var credential string
	require.NoError(t, p.db.QueryRow(`SELECT key FROM api_keys WHERE id=$1`, p.key).Scan(&credential))
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary, ID: p.peer.ID, Token: p.peer.Token}}
	svc := service.NewAPIKeyService(NewAPIKeyRepository(nil, p.replica), nil, nil, nil, nil, nil, cfg)
	initial, err := svc.GetByKey(ctx, credential)
	require.NoError(t, err)
	_, err = p.db.Exec(`UPDATE users SET concurrency=7,platform_concurrency_limits='{"deepseek":3}' WHERE id=$1`, p.user)
	require.NoError(t, err)
	snap, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: p.version})
	require.NoError(t, err)
	response := site.SyncResponse{PrimaryID: "main", Full: snap.Full, BaseVersion: p.version, Version: snap.Version, Rows: snap.Rows}
	installCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := false
	p.local.BeforeAuthCommit = func() func() {
		finish := svc.BeginSiteAuthInstall()
		cancel() // Roll back the actual database transaction at its commit fence.
		return func() { finish(); finished = true }
	}
	require.Error(t, p.local.Install(installCtx, response))
	require.True(t, finished)
	old, err := svc.GetByKey(ctx, credential)
	require.NoError(t, err)
	require.Equal(t, initial.User.Concurrency, old.User.Concurrency)
	require.Equal(t, initial.User.PlatformConcurrencyLimits, old.User.PlatformConcurrencyLimits)
	require.Equal(t, 1, svc.AuthLookupMetrics().SecondaryCacheEntries, "rollback must not leave auth permanently bypassed")
	p.local.BeforeAuthCommit = svc.BeginSiteAuthInstall
	require.NoError(t, p.local.Install(ctx, response))
	updated, err := svc.GetByKey(ctx, credential)
	require.NoError(t, err)
	require.Equal(t, 7, updated.User.Concurrency)
	require.Equal(t, map[string]int{"deepseek": 3}, updated.User.PlatformConcurrencyLimits)
}
