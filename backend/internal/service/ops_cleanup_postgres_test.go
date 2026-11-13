//go:build retentionintegration

package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/testutil/retentionpg"
	"github.com/stretchr/testify/require"
)

type retentionSettings struct {
	SettingRepository
	policy OpsDataRetentionSettings
}

type retentionHeartbeats struct {
	OpsRepository
	writes int
}

func (r *retentionHeartbeats) UpsertJobHeartbeat(context.Context, *OpsUpsertJobHeartbeatInput) error {
	r.writes++
	return nil
}

func (r retentionSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	b, err := json.Marshal(OpsAdvancedSettings{DataRetention: r.policy})
	return map[string]string{SettingKeyOpsAdvancedSettings: string(b)}, err
}

func (r retentionSettings) GetValue(context.Context, string) (string, error) {
	return "", ErrSettingNotFound
}

func retentionWorker(db *sql.DB, enabled bool) *OpsCleanupService {
	policy := defaultOpsAdvancedSettings().DataRetention
	policy.CleanupEnabled = enabled
	// This dataset explicitly exercises a short, administrator-selected policy.
	policy.RequestConnectionRetentionDays = 3
	policy.RequestTimingRetentionDays = 3
	policy.BillingDedupArchiveRetentionDays = 15
	cfg := &config.Config{}
	cfg.DashboardAgg.Enabled = true
	cfg.DashboardAgg.Retention.UsageLogsDays = 30
	cfg.DashboardAgg.Retention.UsageBillingDedupDays = 30
	svc := NewOpsCleanupService(nil, db, nil, cfg, nil, retentionSettings{policy: policy})
	svc.refreshEffectiveBeforeRun(context.Background())
	return svc
}

func TestRetentionPostgresBoundaries(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t.Run("timing multiple batches and exact cutoff", func(t *testing.T) {
		db := retentionpg.Private(t, "request_timing_details")
		retentionpg.Exec(t, db, `INSERT INTO request_timing_details(usage_log_id,trace_id,created_at,detail)
		 SELECT i,md5(i::text)::uuid,$1::timestamptz-interval '1 microsecond','{}' FROM generate_series(1,10003) i`, cutoff)
		retentionpg.Exec(t, db, `INSERT INTO request_timing_details VALUES
		 (20001,md5('boundary')::uuid,$1,'{}'),(20002,md5('future')::uuid,$1::timestamptz+interval '1 microsecond','{}')`, cutoff)
		n, err := deleteOldRowsByCTID(ctx, db, "request_timing_details", "created_at", cutoff, "", 5000)
		require.NoError(t, err)
		require.EqualValues(t, 10003, n)
		require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT count(*) FROM request_timing_details`))
		n, err = deleteOldRowsByCTID(ctx, db, "request_timing_details", "created_at", cutoff, "", 5000)
		require.NoError(t, err)
		require.Zero(t, n)
	})
	t.Run("unfinished and null completion protected", func(t *testing.T) {
		db := retentionpg.Private(t, "usage_billing_events")
		retentionpg.Exec(t, db, `INSERT INTO usage_billing_events(request_id,api_key_id,request_fingerprint,schema_version,payload,state,observed_at,completed_at)
		 SELECT state||'-'||age,1,'fp',1,'{}',state,$1::timestamptz-interval '100 days',
		 CASE age WHEN 'old' THEN $1::timestamptz-interval '1 microsecond' WHEN 'boundary' THEN $1::timestamptz WHEN 'new' THEN $1::timestamptz+interval '1 microsecond' END
		 FROM unnest(ARRAY['pending','applied','complete','legacy','blocked']) state
		 CROSS JOIN unnest(ARRAY['old','boundary','new','null']) age`, cutoff)
		n, err := deleteOldRowsByCTID(ctx, db, "usage_billing_events", "completed_at", cutoff, "state IN ('complete','legacy')", 1)
		require.NoError(t, err)
		require.EqualValues(t, 2, n)
		require.EqualValues(t, 18, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_events`))
		require.EqualValues(t, 12, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_events WHERE state IN ('pending','applied','blocked')`))
	})
	t.Run("dedup original time and composite identity", func(t *testing.T) {
		db := retentionpg.Private(t, "usage_billing_dedup", "usage_billing_dedup_archive")
		retentionpg.Exec(t, db, `INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint,created_at)
		 SELECT 'batch-'||i,1,md5(i::text),$1::timestamptz-interval '1 day' FROM generate_series(1,10003) i`, cutoff)
		retentionpg.Exec(t, db, `INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint,created_at) VALUES
		 ('same',1,'fp1',$1::timestamptz-interval '1 day'),('same',2,'fp2',$1::timestamptz-interval '1 day'),
		 ('edge',1,'edge',$1),('future',1,'future',$1::timestamptz+interval '1 microsecond')`, cutoff)
		retentionpg.Exec(t, db, `INSERT INTO usage_billing_dedup_archive(request_id,api_key_id,request_fingerprint,created_at)
		 VALUES('same',1,'fp1',$1::timestamptz-interval '1 day')`, cutoff)
		n, err := archiveOldUsageBillingDedup(ctx, db, cutoff, 5000)
		require.NoError(t, err)
		require.EqualValues(t, 10005, n)
		require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup`))
		require.EqualValues(t, 10005, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup_archive WHERE created_at=$1::timestamptz-interval '1 day'`, cutoff))
		require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup_archive WHERE request_id='same' AND request_fingerprint='fp'||api_key_id`))
		n, err = archiveOldUsageBillingDedup(ctx, db, cutoff, 5000)
		require.NoError(t, err)
		require.Zero(t, n)
		// The archive's expiry uses original creation, not the time it was moved.
		n, err = deleteOldRowsByCTID(ctx, db, "usage_billing_dedup_archive", "created_at", cutoff.Add(-24*time.Hour), "", 5000)
		require.NoError(t, err)
		require.Zero(t, n)
		n, err = deleteOldRowsByCTID(ctx, db, "usage_billing_dedup_archive", "created_at", cutoff.Add(-24*time.Hour+time.Microsecond), "", 5000)
		require.NoError(t, err)
		require.EqualValues(t, 10005, n)
	})
}

func TestRetentionPostgresRollbackAndCancellation(t *testing.T) {
	db := retentionpg.Private(t, "usage_billing_dedup", "usage_billing_dedup_archive")
	retentionpg.Exec(t, db, `INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint,created_at)
	 SELECT 'rollback-'||i,1,'fp',now()-interval '20 days' FROM generate_series(1,7) i`)
	retentionpg.Exec(t, db, `ALTER TABLE usage_billing_dedup_archive ADD CONSTRAINT fail_archive CHECK(api_key_id <> 1)`)
	n, err := archiveOldUsageBillingDedup(context.Background(), db, time.Now().AddDate(0, 0, -15), 2)
	require.Error(t, err)
	require.Zero(t, n)
	require.EqualValues(t, 7, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup`))
	require.Zero(t, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup_archive`))
	retentionpg.Exec(t, db, `ALTER TABLE usage_billing_dedup_archive DROP CONSTRAINT fail_archive`)
	// Hold a lock so cancellation interrupts a real in-flight database statement.
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`LOCK TABLE usage_billing_dedup_archive IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = archiveOldUsageBillingDedup(ctx, db, time.Now().AddDate(0, 0, -15), 2)
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.NoError(t, tx.Rollback())
	require.EqualValues(t, 7, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup`))
	n, err = archiveOldUsageBillingDedup(context.Background(), db, time.Now().AddDate(0, 0, -15), 2)
	require.NoError(t, err)
	require.EqualValues(t, 7, n)
	retentionpg.Exec(t, db, `VACUUM (ANALYZE) usage_billing_dedup_archive`)
	require.Zero(t, retentionpg.Count(t, db, `SELECT count(*) FROM pg_index WHERE indrelid='usage_billing_dedup_archive'::regclass AND NOT indisvalid`))
	_, err = db.Exec(`INSERT INTO usage_billing_dedup_archive SELECT * FROM usage_billing_dedup_archive LIMIT 1`)
	require.Error(t, err, "unique index must still enforce identity after cleanup/vacuum")
}

func TestRetentionPostgresDisabled(t *testing.T) {
	db := retentionpg.Private(t, "request_timing_details")
	retentionpg.Exec(t, db, `INSERT INTO request_timing_details VALUES(1,md5('disabled')::uuid,now()-interval '20 days','{}')`)
	svc := retentionWorker(db, false)
	counts, err := svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, opsCleanupDeletedCounts{}, counts)
	require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM request_timing_details`))
	// Simulate an old cron still firing after a different instance disabled
	// cleanup in shared settings. It must neither delete nor record a run.
	heartbeats := &retentionHeartbeats{}
	svc.opsRepo = heartbeats
	svc.cfg.RunMode = config.RunModeSimple
	svc.effective.Enabled = true
	svc.runScheduled()
	require.Zero(t, heartbeats.writes)
	require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM request_timing_details`))
}

func TestRetentionPostgresLeaderLock(t *testing.T) {
	db := retentionpg.Open(t)
	first, second := retentionWorker(db, true), retentionWorker(db, true)
	release, acquired := first.tryAcquireLeaderLock(context.Background())
	require.True(t, acquired)
	require.NotNil(t, release)
	defer release()
	_, acquired = second.tryAcquireLeaderLock(context.Background())
	require.False(t, acquired, "a second instance must not run cleanup concurrently")
	release()
	releaseSecond, acquired := second.tryAcquireLeaderLock(context.Background())
	require.True(t, acquired)
	require.NotNil(t, releaseSecond)
	releaseSecond()
}

// Uses the real migrated tables, including triggers and foreign keys. The runner
// seeds them, dumps before/after, restores, and verifies billing replay.
func TestRetentionPostgresWorkerDataset(t *testing.T) {
	db := retentionpg.Open(t)
	svc := retentionWorker(db, true)
	counts, err := svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 57000, counts.requestConnections)
	require.EqualValues(t, 57000, counts.requestTimings)
	require.EqualValues(t, 53000, counts.billingEvents)
	require.EqualValues(t, 53000, counts.userTraffic)
	require.EqualValues(t, 45001, counts.billingDedupArchived)
	require.EqualValues(t, 30000, counts.billingDedupDeleted)
	for _, query := range []string{
		`SELECT count(*) FROM request_connection_traces WHERE started_at < now()-interval '3 days'`,
		`SELECT count(*) FROM request_timing_details WHERE created_at < now()-interval '3 days'`,
		`SELECT count(*) FROM user_traffic_events WHERE started_at < now()-interval '7 days'`,
		`SELECT count(*) FROM usage_billing_events WHERE state IN ('complete','legacy') AND completed_at < now()-interval '7 days'`,
		`SELECT count(*) FROM usage_billing_dedup WHERE created_at < now()-interval '15 days'`,
		`SELECT count(*) FROM usage_billing_dedup_archive WHERE created_at < now()-interval '30 days'`,
	} {
		require.Zero(t, retentionpg.Count(t, db, query), query)
	}
	require.EqualValues(t, 3, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_events WHERE state IN ('pending','applied','blocked')`))
	require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM user_traffic_hourly`))
	require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM usage_logs WHERE request_id='retention-usage-control'`))
	require.EqualValues(t, 100, retentionpg.Count(t, db, `SELECT balance::bigint FROM users WHERE id=910001`))
	counts, err = svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, opsCleanupDeletedCounts{}, counts, "repeated cleanup must be idempotent")
}
