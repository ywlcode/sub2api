package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// The exact eligibility rules and usage-row timestamp match the user query.
func globalPhaseEligibleCTE(where string) string {
	return fmt.Sprintf(`WITH scoped AS (
  SELECT id FROM usage_logs %s
), boundaries AS (
  SELECT d.usage_log_id,
    CASE WHEN jsonb_typeof(detail->'events'->'body_read_start')='number'
      THEN (detail->'events'->>'body_read_start')::numeric END AS started,
    CASE WHEN jsonb_typeof(detail->'events'->'body_received')='number'
      THEN (detail->'events'->>'body_received')::numeric END AS received,
    CASE WHEN jsonb_typeof(detail->'total_ms')='number'
      THEN (detail->>'total_ms')::numeric END AS finished,
    detail->'body_complete'='true'::jsonb
      AND NOT COALESCE(detail->'events' ? 'body_read_error', false) AS complete
  FROM request_timing_details d JOIN scoped u ON u.id=d.usage_log_id
), requests AS (
  SELECT usage_log_id, MIN(started) AS started, MIN(received) AS received, MIN(finished) AS finished
  FROM boundaries GROUP BY usage_log_id HAVING COUNT(*)=1 AND BOOL_AND(complete)
), eligible AS (
  SELECT received-started AS upload_ms, finished-received AS processing_ms
  FROM requests WHERE started>=0 AND received>=started AND finished>=received
)`, where)
}

// SeedGlobalRequestPhaseHourBuckets queues only missing completed UTC hours.
func (r *usageLogRepository) SeedGlobalRequestPhaseHourBuckets(ctx context.Context, now time.Time) error {
	hour := now.UTC().Truncate(time.Hour)
	_, err := r.sql.ExecContext(ctx, `INSERT INTO usage_request_phase_hourly_dirty(bucket_start)
SELECT hours.bucket_start
FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 hour') AS hours(bucket_start)
WHERE NOT EXISTS (SELECT 1 FROM usage_request_phase_hourly_global b WHERE b.bucket_start=hours.bucket_start)
ON CONFLICT (bucket_start) DO NOTHING`, hour.Add(-24*time.Hour), hour.Add(-time.Hour))
	if err != nil {
		return err
	}
	cutoff := hour.Add(-24 * time.Hour)
	if _, err = r.sql.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start < $1`, cutoff); err != nil {
		return err
	}
	_, err = r.sql.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_global WHERE bucket_start < $1`, cutoff)
	return err
}

// Keep the dirty row locked until recomputation commits. A concurrent trigger
// then marks the same hour again rather than losing a late timing change.
func (r *usageLogRepository) RefreshOneGlobalRequestPhaseHourBucket(ctx context.Context, now time.Time) (bool, error) {
	if r.db == nil {
		return false, errors.New("global request-phase buckets require a database connection")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	hour := now.UTC().Truncate(time.Hour)
	var bucket time.Time
	err = tx.QueryRowContext(ctx, `SELECT bucket_start FROM usage_request_phase_hourly_dirty
WHERE bucket_start >= $1 AND bucket_start < $2
ORDER BY bucket_start LIMIT 1 FOR UPDATE SKIP LOCKED`, hour.Add(-24*time.Hour), hour).Scan(&bucket)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = recomputeGlobalRequestPhaseHour(ctx, tx, bucket); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, bucket); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func recomputeGlobalRequestPhaseHour(ctx context.Context, exec sqlExecutor, bucket time.Time) error {
	query := globalPhaseEligibleCTE(`WHERE created_at >= $1 AND created_at < $2`) + `
INSERT INTO usage_request_phase_hourly_global
  (bucket_start, upload_sum_ms, processing_sum_ms, sample_count, computed_at)
SELECT $1, COALESCE(SUM(upload_ms),0), COALESCE(SUM(processing_ms),0), COUNT(*), NOW()
FROM eligible
ON CONFLICT (bucket_start) DO UPDATE SET
  upload_sum_ms=EXCLUDED.upload_sum_ms,
  processing_sum_ms=EXCLUDED.processing_sum_ms,
  sample_count=EXCLUDED.sample_count,
  computed_at=EXCLUDED.computed_at`
	_, err := exec.ExecContext(ctx, query, bucket, bucket.Add(time.Hour))
	return err
}

// Completed middle hours come from buckets. The partial boundary hours are
// scanned directly, preserving the original exact rolling 24-hour window.
func (r *usageLogRepository) ReadGlobalRequestPhaseHourlyAverages(ctx context.Context, start, end time.Time) (usagestats.RequestPhaseAverages, error) {
	start, end = start.UTC(), end.UTC()
	if !end.After(start) {
		return usagestats.RequestPhaseAverages{}, errors.New("invalid global request-phase window")
	}
	fullStart := start.Truncate(time.Hour)
	if fullStart.Before(start) {
		fullStart = fullStart.Add(time.Hour)
	}
	fullEnd := end.Truncate(time.Hour)
	expectedBuckets := int64(fullEnd.Sub(fullStart) / time.Hour)
	var bucketCount int64
	if err := scanSingleRow(ctx, r.sql, `SELECT COUNT(*) FROM usage_request_phase_hourly_global WHERE bucket_start >= $1 AND bucket_start < $2`,
		[]any{fullStart, fullEnd}, &bucketCount); err != nil {
		return usagestats.RequestPhaseAverages{}, err
	}
	if bucketCount != expectedBuckets {
		return usagestats.RequestPhaseAverages{}, fmt.Errorf("global request-phase buckets incomplete: have %d of %d", bucketCount, expectedBuckets)
	}
	query := globalPhaseEligibleCTE(`WHERE (created_at >= $1 AND created_at < $2)
  OR (created_at >= $3 AND created_at < $4)`) + `,
edge_totals AS (
  SELECT COALESCE(SUM(upload_ms),0) AS upload_sum,
    COALESCE(SUM(processing_ms),0) AS processing_sum, COUNT(*) AS samples FROM eligible
), bucket_totals AS (
  SELECT COALESCE(SUM(upload_sum_ms),0) AS upload_sum,
    COALESCE(SUM(processing_sum_ms),0) AS processing_sum,
    COALESCE(SUM(sample_count),0)::bigint AS samples
  FROM usage_request_phase_hourly_global WHERE bucket_start >= $5 AND bucket_start < $6
)
SELECT CASE WHEN b.samples+e.samples > 0 THEN (b.upload_sum+e.upload_sum)/(b.samples+e.samples) END,
  CASE WHEN b.samples+e.samples > 0 THEN (b.processing_sum+e.processing_sum)/(b.samples+e.samples) END,
  b.samples+e.samples
FROM bucket_totals b CROSS JOIN edge_totals e`
	var stats usagestats.RequestPhaseAverages
	err := scanSingleRow(ctx, r.sql, query,
		[]any{start, fullStart, fullEnd, end, fullStart, fullEnd},
		&stats.AverageUserUploadMs, &stats.AverageProcessingMs, &stats.TimingSampleCount)
	if err != nil {
		return usagestats.RequestPhaseAverages{}, err
	}
	return stats, nil
}
