//go:build retentionintegration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/testutil/retentionpg"
	"github.com/stretchr/testify/require"
)

func TestRetentionPostgresHistoryBatches(t *testing.T) {
	t.Run("monitor tiers preserve bootstrap and exact boundaries", func(t *testing.T) {
		db := retentionpg.Private(t)
		retentionpg.Exec(t, db, `CREATE TABLE channel_monitor_v2_watermarks(id bigint,backfill_cursor timestamptz)`)
		created := map[string]bool{}
		now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		for _, rule := range channelMonitorV2RetentionRules {
			if !created[rule.table] {
				retentionpg.Exec(t, db, fmt.Sprintf(`CREATE TABLE %s(bucket_start timestamptz,bucket_seconds int)`, rule.table))
				created[rule.table] = true
			}
			cutoff := channelMonitorV2RetentionCutoff(now, rule.retention)
			_, err := db.Exec(`INSERT INTO `+rule.table+` SELECT $1::timestamptz-interval '1 minute',$2 FROM generate_series(1,10001)`, cutoff, rule.bucketSeconds)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO `+rule.table+` VALUES($1,$2),($1::timestamptz+interval '1 minute',$2)`, cutoff, rule.bucketSeconds)
			require.NoError(t, err)
		}
		repo := &channelMonitorV2Repository{db: db}
		// Missing/null/incomplete cursors never allow deletion of backfill facts.
		require.NoError(t, repo.PruneRetention(context.Background(), now))
		retentionpg.Exec(t, db, `INSERT INTO channel_monitor_v2_watermarks VALUES(1,NULL)`)
		require.NoError(t, repo.PruneRetention(context.Background(), now))
		_, err := db.Exec(`UPDATE channel_monitor_v2_watermarks SET backfill_cursor=$1`, now)
		require.NoError(t, err)
		require.NoError(t, repo.PruneRetention(context.Background(), now))
		require.EqualValues(t, 10003, retentionpg.Count(t, db, `SELECT count(*) FROM channel_monitor_v2_metrics_1m`))
		_, err = db.Exec(`UPDATE channel_monitor_v2_watermarks SET backfill_cursor=$1`, now.Add(-channelMonitorV2RetentionMax))
		require.NoError(t, err)
		require.NoError(t, repo.PruneRetention(context.Background(), now))
		for _, rule := range channelMonitorV2RetentionRules {
			require.EqualValues(t, 2, retentionpg.Count(t, db, fmt.Sprintf(`SELECT count(*) FROM %s WHERE bucket_seconds=%d`, rule.table, rule.bucketSeconds)))
		}
	})
	t.Run("showcase drains expired and excess snapshots across batches", func(t *testing.T) {
		db := retentionpg.Private(t)
		retentionpg.Exec(t, db, `CREATE TABLE pelican_group_test_plans(group_id bigint);
CREATE TABLE pelican_showcase_items(id bigint primary key,group_id bigint,generated_at timestamptz);
INSERT INTO pelican_group_test_plans VALUES(1),(3);
INSERT INTO pelican_showcase_items SELECT i,1,now()-interval '8 days' FROM generate_series(1,12001) i;
INSERT INTO pelican_showcase_items SELECT i,1,now() FROM generate_series(12002,12021) i;
INSERT INTO pelican_showcase_items SELECT i,2,now() FROM generate_series(13001,13010) i`)
		repo := &pelicanShowcaseRepository{db: db}
		require.NoError(t, repo.Prune(context.Background(), 3, time.Now().Add(-7*24*time.Hour)))
		require.EqualValues(t, 3, retentionpg.Count(t, db, `SELECT count(*) FROM pelican_showcase_items`))
		require.EqualValues(t, 12019, retentionpg.Count(t, db, `SELECT min(id) FROM pelican_showcase_items`))
		retentionpg.Exec(t, db, `INSERT INTO pelican_showcase_items VALUES(14001,3,now()-interval '30 days')`)
		require.NoError(t, repo.Prune(context.Background(), 3, time.Time{}))
		require.EqualValues(t, 4, retentionpg.Count(t, db, `SELECT count(*) FROM pelican_showcase_items`))
	})
	t.Run("probe recovery keeps history until daily cleanup", func(t *testing.T) {
		db := retentionpg.Private(t)
		retentionpg.Exec(t, db, `CREATE TABLE channel_monitor_v2_candy_results(id bigint, verdict text, reason text, checked_at timestamptz, finished_at timestamptz);
INSERT INTO channel_monitor_v2_candy_results VALUES (1,'running','',now()-interval '4 minutes',NULL),(2,'correct','',now()-interval '2 days',now()),(3,'running','',now(),NULL)`)
		repo := &channelMonitorV2Repository{db: db}
		require.NoError(t, repo.RecoverExpiredCandyClaims(context.Background(), time.Now()))
		require.EqualValues(t, 3, retentionpg.Count(t, db, `SELECT count(*) FROM channel_monitor_v2_candy_results`))
		require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM channel_monitor_v2_candy_results WHERE id=1 AND verdict='error' AND reason='interrupted'`))
		require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM channel_monitor_v2_candy_results WHERE id=3 AND verdict='running'`))
		n, err := deleteHistoryBatches(context.Background(), db, "channel_monitor_v2_candy_results", "checked_at", "", time.Now().Add(-24*time.Hour))
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})
	t.Run("scheduled history drains all expired batches only for pelican plans", func(t *testing.T) {
		db := retentionpg.Private(t)
		retentionpg.Exec(t, db, `CREATE TABLE scheduled_test_plans(id bigint primary key,pelican_config jsonb);
CREATE TABLE scheduled_test_results(id bigint,plan_id bigint,created_at timestamptz);
CREATE TABLE pelican_group_test_results(id bigint,created_at timestamptz);
INSERT INTO scheduled_test_plans VALUES(1,'{}'),(2,NULL);
INSERT INTO scheduled_test_results SELECT i,1,now()-interval '8 days' FROM generate_series(1,12001) i;
INSERT INTO scheduled_test_results VALUES(20001,2,now()-interval '8 days'),(20002,1,now());
INSERT INTO pelican_group_test_results SELECT i,now()-interval '8 days' FROM generate_series(1,12001) i;
INSERT INTO pelican_group_test_results VALUES(20001,now())`)
		cutoff := time.Now().Add(-7 * 24 * time.Hour)
		scheduled := &scheduledTestResultRepository{db: db}
		groups := &pelicanGroupTestRepository{db: db}
		require.NoError(t, scheduled.PruneExpiredPelican(context.Background(), cutoff))
		require.NoError(t, groups.PruneExpiredResults(context.Background(), cutoff))
		require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT count(*) FROM scheduled_test_results`))
		require.EqualValues(t, 1, retentionpg.Count(t, db, `SELECT count(*) FROM pelican_group_test_results`))
	})
	t.Run("moderation retains independent hit and pass windows", func(t *testing.T) {
		db := retentionpg.Private(t, "content_moderation_logs")
		retentionpg.Exec(t, db, `INSERT INTO content_moderation_logs(flagged,created_at)
 SELECT true,now()-interval '10 days' FROM generate_series(1,10003);
INSERT INTO content_moderation_logs(flagged,created_at)
 SELECT false,now()-interval '3 days' FROM generate_series(1,9001);
INSERT INTO content_moderation_logs(flagged,created_at) VALUES (true,now()-interval '3 days'),(false,now())`)
		repo := &contentModerationRepository{db: db}
		result, err := repo.CleanupExpiredLogs(context.Background(), time.Now().AddDate(0, 0, -5), time.Now().AddDate(0, 0, -2))
		require.NoError(t, err)
		require.EqualValues(t, 10003, result.DeletedHit)
		require.EqualValues(t, 9001, result.DeletedNonHit)
		require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT count(*) FROM content_moderation_logs`))
	})
	t.Run("dashboard drains multiple batches and preserves boundary", func(t *testing.T) {
		db := retentionpg.Private(t)
		for _, target := range []struct{ table, column, sqlType string }{
			{"usage_dashboard_hourly", "bucket_start", "timestamptz"},
			{"usage_dashboard_hourly_users", "bucket_start", "timestamptz"},
			{"usage_dashboard_daily", "bucket_date", "date"},
			{"usage_dashboard_daily_users", "bucket_date", "date"},
		} {
			retentionpg.Exec(t, db, fmt.Sprintf(`CREATE TABLE %s(%s %s);
INSERT INTO %s SELECT '2026-01-01'::%s FROM generate_series(1,10003);
INSERT INTO %s VALUES ('2026-02-01'),('2026-02-02')`, target.table, target.column, target.sqlType, target.table, target.sqlType, target.table))
		}
		cutoff := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		repo := &dashboardAggregationRepository{sql: db}
		require.NoError(t, repo.CleanupAggregates(context.Background(), cutoff, cutoff))
		for _, table := range []string{"usage_dashboard_hourly", "usage_dashboard_hourly_users", "usage_dashboard_daily", "usage_dashboard_daily_users"} {
			require.EqualValues(t, 2, retentionpg.Count(t, db, "SELECT count(*) FROM "+table))
		}
	})
}
