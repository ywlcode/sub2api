package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BackfillConnectionTraceBodyBytes advances a durable cursor in small batches.
// The progress row serializes replicas without blocking another worker. Invalid
// legacy measurements stay NULL, but still advance the cursor. Zero is measured.
func (r *usageLogRepository) BackfillConnectionTraceBodyBytes(ctx context.Context, limit int) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var before, oldest time.Time
	var after string
	var completed bool
	err = tx.QueryRowContext(ctx, `SELECT before_time,after_trace_id,oldest_time,completed
 FROM connection_trace_body_bytes_backfill WHERE id=1 FOR UPDATE SKIP LOCKED`).Scan(&before, &after, &oldest, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || completed {
		return completed, err
	}
	// Match the existing (started_at DESC, trace_id ASC) index exactly, including
	// ties. Only the bounded batch's JSON is read or updated.
	rows, err := tx.QueryContext(ctx, `WITH batch AS MATERIALIZED (
 SELECT trace_id,started_at,detail FROM request_connection_traces
 WHERE source='application' AND body_bytes IS NULL AND started_at >= $1
 AND started_at <= $2 AND (started_at < $2 OR trace_id > $3::uuid)
 ORDER BY started_at DESC,trace_id ASC LIMIT $4
 ), updated AS (
 UPDATE request_connection_traces t SET body_bytes=(b.detail->>'body_bytes')::numeric::bigint
 FROM batch b WHERE t.trace_id=b.trace_id
 AND CASE WHEN jsonb_typeof(b.detail->'body_bytes')='number'
 THEN (b.detail->>'body_bytes')::numeric BETWEEN 0 AND 9223372036854775807
 AND trunc((b.detail->>'body_bytes')::numeric)=(b.detail->>'body_bytes')::numeric ELSE false END
 RETURNING t.trace_id
 ) SELECT started_at,trace_id FROM batch ORDER BY started_at ASC,trace_id DESC LIMIT 1`, oldest, before, after, limit)
	if err != nil {
		return false, err
	}
	found := rows.Next()
	if found {
		err = rows.Scan(&before, &after)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE connection_trace_body_bytes_backfill
 SET before_time=$1,after_trace_id=$2,completed=$3 WHERE id=1`, before, after, !found)
	if err != nil {
		return false, err
	}
	return !found, tx.Commit()
}
