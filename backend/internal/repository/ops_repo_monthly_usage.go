package repository

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/monthlyusage"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.OpsMonthlyUsageRepository = (*opsRepository)(nil)

// Keep these SELECTs aligned with deploy/monthly-usage/{headcounts,usage}.sql.
// The deploy versions also contain psql variables and COPY formatting.
//
//go:embed monthly_usage_sql/headcounts.sql
var monthlyHeadcountsSQL string

//go:embed monthly_usage_sql/usage.sql
var monthlyUsageSQL string

const monthlyUsageEmailPattern = `^[[:space:]]*(".*"|[^@[:space:]]+)@[^@[:space:]]+[[:space:]]*$`

func monthlyReportDate(month string) (string, error) {
	t, err := time.Parse("2006-01", month)
	if err != nil || t.Year() < 1 {
		return "", errors.New("invalid report month (expected YYYY-MM)")
	}
	return month + "-01", nil
}

// MonthlyUsageDepartmentAttributeAvailable checks an explicit attribute definition.
func (r *opsRepository) MonthlyUsageDepartmentAttributeAvailable(ctx context.Context, id int64) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("missing monthly report database")
	}
	return monthlyUsageDepartmentAttributeAvailable(ctx, r.db, id)
}

func monthlyUsageDepartmentAttributeAvailable(ctx context.Context, db sqlQueryer, id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	var available bool
	err := scanSingleRow(ctx, db, `SELECT EXISTS (
		SELECT 1 FROM user_attribute_definitions
		WHERE id = $1 AND deleted_at IS NULL AND enabled AND type IN ('text', 'textarea', 'select')
	)`, []any{id}, &available)
	return available, err
}

func (r *opsRepository) BuildMonthlyUsageReport(ctx context.Context, month, site string, departmentAttributeID int64, w io.Writer) error {
	_, err := r.BuildMonthlyUsageReportWithCoverage(ctx, month, site, departmentAttributeID, w)
	return err
}

// BuildMonthlyUsageReportWithCoverage returns the notice printed in the attachment
// so the sender can include the same snapshot's coverage in the message body.
func (r *opsRepository) BuildMonthlyUsageReportWithCoverage(ctx context.Context, month, site string, departmentAttributeID int64, w io.Writer) (string, error) {
	date, err := monthlyReportDate(month)
	if err != nil {
		return "", err
	}
	if r == nil || r.db == nil || w == nil {
		return "", errors.New("missing monthly report database or writer")
	}

	// Keep the MVCC snapshot only while reading the DB. Spool on disk so the
	// largest month fits without buffering all rows or waiting on a slow writer.
	file, err := os.CreateTemp("", "sub2api-monthly-usage-*")
	if err != nil {
		return "", err
	}
	if err := os.Remove(file.Name()); err != nil {
		// Some platforms only allow unlink after close.
		defer os.Remove(file.Name())
	}
	defer file.Close()

	snapshot, err := r.snapshotMonthlyUsage(ctx, date, departmentAttributeID, file)
	if err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	err = monthlyusage.BuildContext(ctx, w, site, snapshot.registered, snapshot.notice, func(visit func(monthlyusage.Row) error) error {
		decoder := gob.NewDecoder(bufio.NewReader(file))
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var row monthlyusage.Row
			if err := decoder.Decode(&row); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			if err := visit(row); err != nil {
				return err
			}
		}
	})
	if err != nil {
		return "", err
	}
	return snapshot.notice, nil
}

func (r *opsRepository) snapshotMonthlyUsage(ctx context.Context, date string, departmentAttributeID int64, dest io.Writer) (*monthlyUsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if departmentAttributeID != 0 {
		ok, err := monthlyUsageDepartmentAttributeAvailable(ctx, tx, departmentAttributeID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("monthly usage department attribute %d is unavailable", departmentAttributeID)
		}
	}

	var raw []byte
	if err := tx.QueryRowContext(ctx, monthlyHeadcountsSQL, date, departmentAttributeID).Scan(&raw); err != nil {
		return nil, err
	}
	var registered map[string]int64
	if err := json.Unmarshal(raw, &registered); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, monthlyUsageSQL, date, departmentAttributeID, monthlyUsageEmailPattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buffer := bufio.NewWriter(dest)
	encoder := gob.NewEncoder(buffer)
	var firstDay, lastDay string
	for rows.Next() {
		var row monthlyusage.Row
		var username, realName, department, groups sql.NullString
		if err := rows.Scan(&row.Day, &row.UserID, &username, &realName, &department, &groups,
			&row.Model, &row.Requests, &row.InputTokens, &row.OutputTokens, &row.CacheReadTokens,
			&row.TotalTokens, &row.ActualCost); err != nil {
			return nil, err
		}
		if firstDay == "" {
			firstDay = row.Day
		}
		lastDay = row.Day
		row.Username, row.RealName = username.String, realName.String
		row.Department, row.Groups = department.String, groups.String
		if err := encoder.Encode(row); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	notice, err := monthlyCoverageNotice(ctx, tx, date, firstDay, lastDay)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &monthlyUsageSnapshot{registered: registered, notice: notice}, buffer.Flush()
}

func monthlyDeliveryKey(month, recipient string) (string, []byte, error) {
	date, err := monthlyReportDate(month)
	if err != nil {
		return "", nil, err
	}
	recipient = strings.ToLower(strings.TrimSpace(recipient))
	if recipient == "" {
		return "", nil, errors.New("missing monthly report recipient")
	}
	hash := sha256.Sum256([]byte(recipient))
	return date, hash[:], nil
}

// Claims left by a crash (including rows from migration 388) become visibly
// uncertain after the worker deadline plus grace. Only a recorded rejection
// permits another SMTP attempt.
func (r *opsRepository) MonthlyUsageDelivery(ctx context.Context, month, recipient string) (service.MonthlyUsageDeliveryState, error) {
	date, hash, err := monthlyDeliveryKey(month, recipient)
	if err != nil {
		return "", err
	}
	var state service.MonthlyUsageDeliveryState
	err = r.db.QueryRowContext(ctx, `
SELECT CASE WHEN accepted_at IS NOT NULL THEN 'accepted'
            WHEN rejected_at IS NOT NULL THEN 'rejected'
            WHEN uncertain_at IS NOT NULL OR claimed_at < now() - interval '30 minutes' THEN 'uncertain'
            ELSE 'sending' END
FROM monthly_usage_email_deliveries WHERE report_month = $1 AND recipient_hash = $2`, date, hash).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return service.MonthlyUsageUnclaimed, nil
	}
	return state, err
}

// New due months take priority. Otherwise rotate through old rejections by
// last attempt, considering only addresses still on the saved report list.
func (r *opsRepository) NextMonthlyUsageRetry(ctx context.Context, first, last string, recipients []string) (string, error) {
	hashes := make(pq.ByteaArray, 0, len(recipients))
	for _, addr := range recipients {
		_, hash, err := monthlyDeliveryKey(first, addr)
		if err != nil {
			return "", err
		}
		hashes = append(hashes, hash)
	}
	var month string
	err := r.db.QueryRowContext(ctx, `
SELECT to_char(report_month, 'YYYY-MM') FROM monthly_usage_email_deliveries
WHERE rejected_at IS NOT NULL AND report_month BETWEEN $1 AND $2 AND recipient_hash = ANY($3::bytea[])
ORDER BY rejected_at, report_month LIMIT 1`, first+"-01", last+"-01", hashes).Scan(&month)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return month, err
}

func (r *opsRepository) AdvanceMonthlyUsageDispatch(ctx context.Context, period, configValue string) (bool, error) {
	// Serialize with settings edits. An addition committed after our final
	// recipient scan must keep the month open. A slower worker cannot rewind it.
	res, err := r.db.ExecContext(ctx, `
INSERT INTO settings (key, value)
SELECT $1, $2 FROM settings WHERE key = $3 AND value = $4 FOR SHARE
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
WHERE settings.value < EXCLUDED.value`, service.MonthlyUsageDispatchCheckpoint, period, service.SettingKeyOpsEmailNotificationConfig, configValue)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *opsRepository) ClaimMonthlyUsageEmail(ctx context.Context, month, recipient string) (bool, error) {
	return r.claimMonthlyUsageEmail(ctx, month, recipient, nil)
}

func (r *opsRepository) ClaimMonthlyUsageEmailForConfig(ctx context.Context, month, recipient, configValue string) (bool, error) {
	return r.claimMonthlyUsageEmail(ctx, month, recipient, &configValue)
}

func (r *opsRepository) claimMonthlyUsageEmail(ctx context.Context, month, recipient string, configValue *string) (bool, error) {
	date, hash, err := monthlyDeliveryKey(month, recipient)
	if err != nil {
		return false, err
	}
	res, err := r.db.ExecContext(ctx, `
INSERT INTO monthly_usage_email_deliveries (report_month, recipient_hash)
SELECT $1::date, $2::bytea WHERE $3::text IS NULL OR EXISTS (
    SELECT 1 FROM settings WHERE key = $4 AND value = $3 FOR SHARE
)
ON CONFLICT (report_month, recipient_hash) DO UPDATE
SET claimed_at = now(), rejected_at = NULL
WHERE monthly_usage_email_deliveries.rejected_at IS NOT NULL`, date, hash, configValue, service.SettingKeyOpsEmailNotificationConfig)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *opsRepository) FinishMonthlyUsageEmail(ctx context.Context, month, recipient string, result service.MonthlyUsageDeliveryState) error {
	date, hash, err := monthlyDeliveryKey(month, recipient)
	if err != nil {
		return err
	}
	switch result {
	case service.MonthlyUsageAccepted, service.MonthlyUsageRejected, service.MonthlyUsageUncertain:
	default:
		return fmt.Errorf("invalid monthly delivery result %q", result)
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE monthly_usage_email_deliveries
SET accepted_at = CASE WHEN $3 = 'accepted' THEN now() END,
    rejected_at = CASE WHEN $3 = 'rejected' THEN now() END,
    uncertain_at = CASE WHEN $3 = 'uncertain' THEN now() END
WHERE report_month = $1 AND recipient_hash = $2
AND accepted_at IS NULL AND rejected_at IS NULL AND uncertain_at IS NULL`, date, hash, result)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("monthly usage email claim missing or already finalized")
	}
	return err
}
