package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// ReadUsageRequestPhaseAverages uses retained, measured request traces rather
// than subtracting unrelated legacy gateway timers. Multiple traces attached to
// an idempotently billed row are ambiguous and do not enter either average.
func (r *usageLogRepository) ReadUsageRequestPhaseAverages(ctx context.Context, filters UsageLogFilters) (usagestats.RequestPhaseAverages, error) {
	conditions, args := usageStatsWhereConditions(filters)
	query := fmt.Sprintf(`
 WITH scoped AS (SELECT id FROM usage_logs %s),
 boundaries AS (
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
 )
 SELECT AVG(received-started), AVG(finished-received), COUNT(*)
 FROM requests WHERE started>=0 AND received>=started AND finished>=received`, buildWhere(conditions))
	var stats usagestats.RequestPhaseAverages
	err := scanSingleRow(ctx, r.sql, query, args, &stats.AverageUserUploadMs, &stats.AverageProcessingMs, &stats.TimingSampleCount)
	return stats, err
}

// Enumerate users in bounded pages, including users whose cache is still cold.
func (r *usageLogRepository) ListRequestPhaseUserIDs(ctx context.Context, afterID int64, limit int) ([]int64, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT id FROM users WHERE id > $1 AND deleted_at IS NULL ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
