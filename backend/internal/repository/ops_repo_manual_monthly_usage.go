package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.OpsManualMonthlyUsageRepository = (*opsRepository)(nil)

const manualMonthlyUsageColumns = `request_id::text, to_char(report_month, 'YYYY-MM'),
	status, total, accepted, failed, uncertain`

func scanManualMonthlyUsage(row *sql.Row) (*service.OpsMonthlyUsageJob, error) {
	var job service.OpsMonthlyUsageJob
	err := row.Scan(&job.ID, &job.Month, &job.Status, &job.Total, &job.Accepted, &job.Failed, &job.Uncertain)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// At most one row matches the partial index. A crashed worker may have sent any
// remaining messages; preserve known outcomes and never restart this request.
func (r *opsRepository) expireManualMonthlyUsage(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE monthly_usage_manual_jobs
SET status = 'uncertain', uncertain = total - accepted - failed, touched_at = now()
WHERE status = 'running' AND touched_at < now() - interval '30 minutes'`)
	return err
}

func (r *opsRepository) BeginManualMonthlyUsage(ctx context.Context, id, month string, total int) (*service.OpsMonthlyUsageJob, bool, error) {
	date, err := monthlyReportDate(month)
	if err != nil {
		return nil, false, err
	}
	if err := r.expireManualMonthlyUsage(ctx); err != nil {
		return nil, false, err
	}
	// Both the primary key and the partial unique index arbitrate across processes.
	job, err := scanManualMonthlyUsage(r.db.QueryRowContext(ctx, `
INSERT INTO monthly_usage_manual_jobs (request_id, report_month, total) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING RETURNING `+manualMonthlyUsageColumns, id, date, total))
	if err == nil {
		return job, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	// A separate statement sees the winning insert even if we waited for its commit.
	job, err = r.readManualMonthlyUsage(ctx, id)
	if errors.Is(err, service.ErrMonthlyUsageJobNotFound) {
		return nil, false, service.ErrMonthlyUsageBusy
	}
	if err != nil {
		return nil, false, err
	}
	if job.Month != month {
		return nil, false, service.ErrMonthlyUsageConflict
	}
	return job, false, nil
}

func (r *opsRepository) readManualMonthlyUsage(ctx context.Context, id string) (*service.OpsMonthlyUsageJob, error) {
	job, err := scanManualMonthlyUsage(r.db.QueryRowContext(ctx, `
SELECT `+manualMonthlyUsageColumns+` FROM monthly_usage_manual_jobs WHERE request_id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrMonthlyUsageJobNotFound
	}
	return job, err
}

func (r *opsRepository) GetManualMonthlyUsage(ctx context.Context, id string) (*service.OpsMonthlyUsageJob, error) {
	if err := r.expireManualMonthlyUsage(ctx); err != nil {
		return nil, err
	}
	return r.readManualMonthlyUsage(ctx, id)
}

func (r *opsRepository) UpdateManualMonthlyUsage(ctx context.Context, job *service.OpsMonthlyUsageJob) error {
	if job == nil {
		return errors.New("missing manual monthly usage job")
	}
	date, err := monthlyReportDate(job.Month)
	if err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE monthly_usage_manual_jobs
SET status = $4, accepted = $5, failed = $6, uncertain = $7, touched_at = now()
WHERE request_id = $1 AND report_month = $2 AND total = $3 AND status = 'running'
  AND touched_at >= now() - interval '30 minutes'
  AND accepted <= $5 AND failed <= $6 AND uncertain <= $7`,
		job.ID, date, job.Total, job.Status, job.Accepted, job.Failed, job.Uncertain)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 1 {
		return err
	}
	current, err := r.GetManualMonthlyUsage(ctx, job.ID)
	if err != nil {
		return err
	}
	// A retried final update after a lost acknowledgement is idempotent.
	if current.Month != job.Month {
		return service.ErrMonthlyUsageConflict
	}
	if current.Status == job.Status && current.Total == job.Total &&
		current.Accepted == job.Accepted && current.Failed == job.Failed && current.Uncertain == job.Uncertain {
		return nil
	}
	return errors.New("manual monthly usage job is terminal or progress is out of date")
}
