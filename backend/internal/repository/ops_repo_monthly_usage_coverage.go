package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

type monthlyUsageSnapshot struct {
	registered map[string]int64
	notice     string
}

// The report and retention cutoffs refer to instants. Dashboard daily buckets
// use the configured aggregation timezone. Compare each overlapping bucket in
// full (all roles on both sides) and name ambiguous boundary days. Counts use the
// usage_logs.created_at index, not a second user/model aggregation.
const monthlyCoverageSQL = `
WITH period AS (
    SELECT $1::date::timestamp AT TIME ZONE 'Asia/Shanghai' AS start_at,
           ($1::date + interval '1 month') AT TIME ZONE 'Asia/Shanghai' AS end_at
), dashboard_days AS (
    SELECT d.total_requests, d.bucket_date,
           d.bucket_date::timestamp AT TIME ZONE $2::text AS start_at,
           (d.bucket_date + 1)::timestamp AT TIME ZONE $2::text AS end_at
    FROM usage_dashboard_daily d
    WHERE d.bucket_date BETWEEN ($1::date - 1) AND ($1::date + interval '1 month')::date
      AND d.total_requests > 0
)
SELECT (SELECT to_char(min(created_at) AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') FROM usage_logs),
       (SELECT CASE WHEN (max(created_at) AT TIME ZONE 'Asia/Shanghai')::date < ($1::date + interval '1 month - 1 day')::date
                    THEN to_char(max(created_at) AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') END
          FROM usage_logs),
       (SELECT CASE WHEN expired_before <= p.start_at THEN '' ELSE to_char(expired_before AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD HH24:MI:SS') END
          FROM usage_group_rollup_state WHERE id = 1),
       EXISTS (SELECT 1 FROM usage_cleanup_tasks c
                WHERE (c.deleted_rows > 0 OR (c.started_at IS NOT NULL AND c.status <> 'succeeded'))
                  AND (c.filters->>'start_time')::timestamptz < p.end_at
                  AND (c.filters->>'end_time')::timestamptz >= p.start_at),
       (SELECT format('%s（%s）日汇总全站 %s 笔，现存 %s 笔%s',
                        to_char(d.bucket_date, 'YYYY-MM-DD'), $2::text, d.total_requests, raw.requests,
                        CASE WHEN d.start_at < p.start_at OR d.end_at > p.end_at THEN '，该日跨北京时间月界' ELSE '' END)
          FROM dashboard_days d
          CROSS JOIN LATERAL (SELECT count(*) AS requests FROM usage_logs l
                               WHERE l.created_at >= d.start_at AND l.created_at < d.end_at) raw
         WHERE d.start_at < p.end_at AND d.end_at > p.start_at
           AND d.total_requests > raw.requests
         ORDER BY (d.start_at < p.start_at OR d.end_at > p.end_at), d.bucket_date LIMIT 1)
FROM period p`

func monthlyCoverageNotice(ctx context.Context, tx *sql.Tx, startDay, firstDay, lastDay string) (string, error) {
	// A daily bucket has no stored timezone. Without an initialized aggregation
	// zone its bounds cannot be reconstructed from the PostgreSQL session zone.
	name := timezone.Name()
	zone := sql.NullString{String: name, Valid: name != "Local"}
	var earliest, shortTail, retained, deficit sql.NullString
	var cleanup bool
	if err := tx.QueryRowContext(ctx, monthlyCoverageSQL, startDay, zone).Scan(&earliest, &shortTail, &retained, &cleanup, &deficit); err != nil {
		return "", err
	}

	var reason string
	switch {
	case deficit.Valid:
		reason = deficit.String
	case retained.Valid && retained.String != "":
		reason = "日志保留边界为北京时间 " + retained.String
	case cleanup:
		reason = "日志清理任务涉及本月"
	case !retained.Valid:
		reason = "缺少日志保留边界"
	case !earliest.Valid:
		reason = "本站账本无用量明细"
	case earliest.String > startDay:
		reason = "本站最早留存明细为 " + earliest.String
	case shortTail.Valid:
		reason = "本站最后留存明细为 " + shortTail.String
	case !zone.Valid:
		reason = "无法确定历史日汇总时区"
	}
	if reason == "" {
		if firstDay == "" {
			return "按现存账本，本月非管理员已入账请求为 0。", nil
		}
		return fmt.Sprintf("本月按现存账本统计，非管理员首末请求日期为 %s 至 %s（北京时间）。", firstDay, lastDay), nil
	}
	if firstDay == "" {
		return "历史覆盖无法确认：" + reason + "。本月未查到非管理员明细，不能认定为零请求。", nil
	}
	return fmt.Sprintf("历史覆盖无法确认：%s。仅统计现存明细（%s 至 %s），用量可能偏低。", reason, firstDay, lastDay), nil
}
