//go:build retentionintegration

package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/testutil/retentionpg"
	"github.com/stretchr/testify/require"
)

func TestRetentionPostgresPartitions(t *testing.T) {
	db := retentionpg.Private(t)
	retentionpg.Exec(t, db, `CREATE TABLE history(id bigint, created_at timestamptz) PARTITION BY RANGE(created_at);
CREATE TABLE history_old PARTITION OF history FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE history_new PARTITION OF history FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
INSERT INTO history VALUES (1,'2025-06-01'),(2,'2026-06-01')`)
	// Both partitions' first rows have ctid (0,1). Deleting by ctid alone
	// would incorrectly delete the retained row from the second partition.
	n, err := deleteOldRowsByCTID(context.Background(), db, "history", "created_at", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "", 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT id FROM history`))
}

func TestRetentionPostgresPartialProgress(t *testing.T) {
	db := retentionpg.Private(t, "request_timing_details")
	retentionpg.Exec(t, db, `INSERT INTO request_timing_details
 SELECT i,md5(i::text)::uuid,now()-interval '40 days'+i*interval '1 second','{}' FROM generate_series(1,5) i;
CREATE FUNCTION fail_third() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF OLD.usage_log_id=3 THEN RAISE EXCEPTION 'injected'; END IF; RETURN OLD; END $$;
CREATE TRIGGER fail_third BEFORE DELETE ON request_timing_details FOR EACH ROW EXECUTE FUNCTION fail_third()`)
	cutoff := time.Now().AddDate(0, 0, -30)
	n, err := deleteOldRowsByCTID(context.Background(), db, "request_timing_details", "created_at", cutoff, "", 2)
	require.ErrorContains(t, err, "injected")
	require.EqualValues(t, 2, n)
	require.EqualValues(t, 3, retentionpg.Count(t, db, `SELECT count(*) FROM request_timing_details`))
	retentionpg.Exec(t, db, `DROP TRIGGER fail_third ON request_timing_details`)
	n, err = deleteOldRowsByCTID(context.Background(), db, "request_timing_details", "created_at", cutoff, "", 2)
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
}

func TestRetentionPostgresTimingVolume(t *testing.T) {
	db := retentionpg.Private(t, "usage_logs", "request_timing_details", "usage_request_phase_hourly_dirty")
	// Copy the actual deployed trigger functions into this isolated schema.
	// This exercises their per-row lookups rather than testing an empty shell.
	for _, name := range []string{"queue_usage_request_phase_hour_dirty", "mark_usage_request_phase_hour_dirty"} {
		var definition string
		require.NoError(t, db.QueryRow(`SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.proname=$1`, name).Scan(&definition))
		retentionpg.Exec(t, db, strings.Replace(definition, "FUNCTION public.", "FUNCTION ", 1))
	}
	retentionpg.Exec(t, db, `CREATE TRIGGER usage_request_phase_detail_dirty AFTER INSERT OR UPDATE OR DELETE ON request_timing_details FOR EACH ROW EXECUTE FUNCTION mark_usage_request_phase_hour_dirty('detail')`)
	retentionpg.Exec(t, db, `INSERT INTO usage_logs(id,user_id,api_key_id,account_id,request_id,model,created_at)
 SELECT i,1,1,1,'volume-'||i,'synthetic',now()-interval '40 days' FROM generate_series(1,500000) i`)
	retentionpg.Exec(t, db, `INSERT INTO request_timing_details
 SELECT CASE WHEN i<=100000 THEN i ELSE i+500000 END,md5(i::text)::uuid,now()-interval '31 days',
 jsonb_build_object('synthetic',repeat(md5(i::text),32)) FROM generate_series(1,200000) i;
INSERT INTO request_timing_details VALUES (1,md5('retained-linked')::uuid,now(),'{}'),(9999999,md5('retained-orphan')::uuid,now(),'{}');
ANALYZE usage_logs; ANALYZE request_timing_details`)
	type writeResult struct {
		err error
		max time.Duration
	}
	writerDone := make(chan writeResult, 1)
	go func() {
		var longest time.Duration
		for i := 0; i < 200; i++ {
			started := time.Now()
			_, err := db.Exec(`INSERT INTO request_timing_details VALUES (2,md5($1)::uuid,now(),'{}')`, fmt.Sprintf("concurrent-%d", i))
			if elapsed := time.Since(started); elapsed > longest {
				longest = elapsed
			}
			if err != nil {
				writerDone <- writeResult{err, longest}
				return
			}
			time.Sleep(time.Millisecond)
		}
		writerDone <- writeResult{nil, longest}
	}()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), opsCleanupStepTimeout)
	defer cancel()
	n, err := deleteOldRowsByCTID(ctx, db, "request_timing_details", "created_at", time.Now().AddDate(0, 0, -30), "", 5000)
	elapsed := time.Since(started)
	writes := <-writerDone
	require.NoError(t, err)
	require.NoError(t, writes.err)
	require.EqualValues(t, 200000, n)
	require.EqualValues(t, 202, retentionpg.Count(t, db, `SELECT count(*) FROM request_timing_details`))
	require.EqualValues(t, 500000, retentionpg.Count(t, db, `SELECT count(*) FROM usage_logs`))
	t.Logf("200000 expired timing rows, 500000 usage rows, real triggers: cleanup=%s rows_per_second=%.0f concurrent_writes=200 max_write_latency=%s", elapsed, float64(n)/elapsed.Seconds(), writes.max)
}
