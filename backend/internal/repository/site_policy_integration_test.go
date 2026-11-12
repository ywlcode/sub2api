//go:build integration

package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestSiteRequestPolicyDependenciesAndQuality(t *testing.T) {
	ctx := context.Background()
	db, client := newBillingAuditDatabase(t)
	replica, _ := newBillingAuditDatabase(t)
	a := mustCreateAccount(t, client, &service.Account{Name: "site-" + uuid.NewString(), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	var profile int64
	require.NoError(t, db.QueryRow(`INSERT INTO tls_fingerprint_profiles(name) VALUES($1) RETURNING id`, uuid.NewString()).Scan(&profile))
	_, err := db.Exec(`UPDATE accounts SET extra=extra||jsonb_build_object('tls_fingerprint_profile_id',$2::bigint) WHERE id=$1`, a.ID, profile)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO account_performance_monitors(account_id,paused) VALUES($1,true)`, a.ID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO settings(key,value) VALUES('enable_metadata_passthrough','true'),('smtp_password','private-only') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`)
	require.NoError(t, err)
	var plan int64
	require.NoError(t, db.QueryRow(`INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,pelican_config,updated_at) VALUES($1,'deepseek-chat','*/30 * * * *',true,'{"quality":{},"parallel_count":1}',NOW()-INTERVAL '1 hour') RETURNING id`, a.ID).Scan(&plan))
	_, err = db.Exec(`INSERT INTO scheduled_test_results(plan_id,status,started_at,finished_at,created_at,pelican_config,quality_round_id,quality_action,quality_judgment) VALUES($1,'success',NOW(),NOW(),NOW(),'{"parallel_count":1}','site-round','passed','{"verdict":"correct"}')`, plan)
	require.NoError(t, err)
	pCfg := &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}}
	pCfg.Billing.MinimumBalanceReserve = 2.75
	primary, err := NewSiteStore(db, pCfg)
	require.NoError(t, err)
	peer := site.Peer{ID: "office", Token: strings.Repeat("x", 32)}
	sCfg := &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}}
	local, err := NewSiteStore(replica, sCfg)
	require.NoError(t, err)
	snap, err := primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	for _, row := range snap.Rows {
		require.NotContains(t, string(row.Value), "private-only")
	}
	require.NoError(t, local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: "1", Rows: snap.Rows}))
	require.Equal(t, 2.75, sCfg.RequestBilling().MinimumBalanceReserve)
	var paused bool
	require.NoError(t, replica.QueryRow(`SELECT NOT schedulable FROM accounts WHERE id=$1`, a.ID).Scan(&paused))
	require.True(t, paused)
	var count int
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM tls_fingerprint_profiles WHERE id=$1`, profile).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM scheduled_test_plans`).Scan(&count))
	require.Zero(t, count, "secondary must not run primary probes")
	q := service.PrioritySchedulingQuery{AccountIDs: []int64{a.ID}, Model: "deepseek-chat", UsageSince: time.Now().Add(-time.Hour), QualitySince: time.Now().Add(-time.Hour)}
	mainSignals, err := newUsageLogRepositoryWithSQL(client, db).ReadPrioritySchedulingSignals(ctx, q)
	require.NoError(t, err)
	localSignals, err := newUsageLogRepositoryWithSQL(nil, replica).ReadPrioritySchedulingSignals(ctx, q)
	require.NoError(t, err)
	require.Equal(t, 1, mainSignals[a.ID].QualitySamples)
	require.Equal(t, mainSignals[a.ID].QualityPassed, localSignals[a.ID].QualityPassed)
	require.Equal(t, mainSignals[a.ID].QualitySamples, localSignals[a.ID].QualitySamples)
	// Latest pause decision is separate from the account's ordinary scheduling flag.
	_, err = db.Exec(`UPDATE account_performance_monitors SET paused=false WHERE account_id=$1`, a.ID)
	require.NoError(t, err)
	snap, err = primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	require.NoError(t, local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: "2", Rows: snap.Rows}))
	require.NoError(t, replica.QueryRow(`SELECT NOT schedulable FROM accounts WHERE id=$1`, a.ID).Scan(&paused))
	require.False(t, paused)

	// Random TLS selection needs the primary's full template set, not a dangling -1 reference.
	_, err = db.Exec(`INSERT INTO tls_fingerprint_profiles(name) VALUES($1)`, uuid.NewString())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{tls_fingerprint_profile_id}','-1') WHERE id=$1`, a.ID)
	require.NoError(t, err)
	snap, err = primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	require.NoError(t, local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: "3", Rows: snap.Rows}))
	var expected int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM tls_fingerprint_profiles`).Scan(&expected))
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM tls_fingerprint_profiles`).Scan(&count))
	require.Equal(t, expected, count)
}
