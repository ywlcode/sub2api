package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

var _ service.OpsManualMonthlyUsageRepository = (*opsRepository)(nil)

const manualMonthlyUsageColumns = `request_id::text, to_char(report_month, 'YYYY-MM'),
	status, total, accepted, failed, uncertain, owner_token`

func scanManualMonthlyUsage(row *sql.Row) (*service.OpsMonthlyUsageJob, uuid.NullUUID, error) {
	var job service.OpsMonthlyUsageJob
	var owner uuid.NullUUID
	err := row.Scan(&job.ID, &job.Month, &job.Status, &job.Total, &job.Accepted, &job.Failed, &job.Uncertain, &owner)
	if err != nil {
		return nil, owner, err
	}
	return &job, owner, nil
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
	// Only this invocation knows the token, including across database/sql retries.
	token := uuid.New()
	insert := func() (*service.OpsMonthlyUsageJob, uuid.NullUUID, error) {
		return scanManualMonthlyUsage(r.db.QueryRowContext(ctx, `
INSERT INTO monthly_usage_manual_jobs (request_id, report_month, total, owner_token) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING RETURNING `+manualMonthlyUsageColumns, id, date, total, token))
	}
	job, owner, insertErr := insert()
	if insertErr != nil && !errors.Is(insertErr, sql.ErrNoRows) && ctx.Err() == nil {
		// The server may still be committing. Repeating the same claim waits
		// for the unique indexes to settle before we read the winning row.
		job, owner, insertErr = insert()
	}
	if insertErr != nil {
		// A claim deadline can expire after commit. Give only the read-back
		// its own short deadline; no fresh write is authorized by this context.
		verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		job, owner, err = r.readManualMonthlyUsage(verifyCtx, id)
		if err != nil {
			if !errors.Is(insertErr, sql.ErrNoRows) {
				// The outcome is unknown. A missing row is not a definitive 404.
				return nil, false, insertErr
			}
			if errors.Is(err, service.ErrMonthlyUsageJobNotFound) {
				return nil, false, service.ErrMonthlyUsageBusy
			}
			return nil, false, err
		}
	}
	if job.Month != month {
		return nil, false, service.ErrMonthlyUsageConflict
	}
	return job, job.Status == "running" && owner.Valid && owner.UUID == token, nil
}

func (r *opsRepository) readManualMonthlyUsage(ctx context.Context, id string) (*service.OpsMonthlyUsageJob, uuid.NullUUID, error) {
	job, owner, err := scanManualMonthlyUsage(r.db.QueryRowContext(ctx, `
SELECT `+manualMonthlyUsageColumns+` FROM monthly_usage_manual_jobs WHERE request_id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrMonthlyUsageJobNotFound
	}
	return job, owner, err
}

func (r *opsRepository) GetManualMonthlyUsage(ctx context.Context, id string) (*service.OpsMonthlyUsageJob, error) {
	if err := r.expireManualMonthlyUsage(ctx); err != nil {
		return nil, err
	}
	job, _, err := r.readManualMonthlyUsage(ctx, id)
	return job, err
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
