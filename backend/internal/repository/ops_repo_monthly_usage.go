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
	"io"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/monthlyusage"
	"github.com/Wei-Shaw/sub2api/internal/service"
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

func (r *opsRepository) BuildMonthlyUsageReport(ctx context.Context, month, site string, w io.Writer) error {
	date, err := monthlyReportDate(month)
	if err != nil {
		return err
	}
	if r == nil || r.db == nil || w == nil {
		return errors.New("missing monthly report database or writer")
	}

	// Keep the MVCC snapshot only while reading the DB. Spool on disk so the
	// largest month fits without buffering all rows or waiting on a slow writer.
	file, err := os.CreateTemp("", "sub2api-monthly-usage-*")
	if err != nil {
		return err
	}
	if err := os.Remove(file.Name()); err != nil {
		// Some platforms only allow unlink after close.
		defer os.Remove(file.Name())
	}
	defer file.Close()

	registered, err := r.snapshotMonthlyUsage(ctx, date, file)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return monthlyusage.Build(w, site, registered, func(visit func(monthlyusage.Row) error) error {
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
}

func (r *opsRepository) snapshotMonthlyUsage(ctx context.Context, date string, dest io.Writer) (map[string]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var raw []byte
	if err := tx.QueryRowContext(ctx, monthlyHeadcountsSQL, date).Scan(&raw); err != nil {
		return nil, err
	}
	var registered map[string]int64
	if err := json.Unmarshal(raw, &registered); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, monthlyUsageSQL, date, monthlyUsageEmailPattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buffer := bufio.NewWriter(dest)
	encoder := gob.NewEncoder(buffer)
	for rows.Next() {
		var row monthlyusage.Row
		var username, realName, department, groups sql.NullString
		if err := rows.Scan(&row.Day, &row.UserID, &username, &realName, &department, &groups,
			&row.Model, &row.Requests, &row.InputTokens, &row.OutputTokens, &row.CacheReadTokens,
			&row.TotalTokens, &row.ActualCost); err != nil {
			return nil, err
		}
		row.Username, row.RealName = username.String, realName.String
		row.Department, row.Groups = department.String, groups.String
		if err := encoder.Encode(row); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return registered, buffer.Flush()
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

// MonthlyUsageEmailClaimed lets callers skip building an attachment for a pending
// or accepted delivery. After a miss, ClaimMonthlyUsageEmail still arbitrates sends.
func (r *opsRepository) MonthlyUsageEmailClaimed(ctx context.Context, month, recipient string) (bool, error) {
	return r.monthlyUsageEmailExists(ctx, month, recipient, `
SELECT EXISTS (SELECT 1 FROM monthly_usage_email_deliveries
               WHERE report_month = $1 AND recipient_hash = $2)`)
}

// MonthlyUsageEmailSettled reports whether this recipient can count toward the
// monthly checkpoint. A 15-minute worker can still release a fresh claim on a
// definite failure. After 30 minutes an unresolved claim is treated as uncertain.
func (r *opsRepository) MonthlyUsageEmailSettled(ctx context.Context, month, recipient string) (bool, error) {
	return r.monthlyUsageEmailExists(ctx, month, recipient, `
SELECT EXISTS (SELECT 1 FROM monthly_usage_email_deliveries
               WHERE report_month = $1 AND recipient_hash = $2
                 AND (accepted_at IS NOT NULL OR claimed_at < now() - interval '30 minutes'))`)
}

func (r *opsRepository) monthlyUsageEmailExists(ctx context.Context, month, recipient, query string) (bool, error) {
	date, hash, err := monthlyDeliveryKey(month, recipient)
	if err != nil {
		return false, err
	}
	if r == nil || r.db == nil {
		return false, errors.New("missing monthly report database")
	}
	var exists bool
	err = r.db.QueryRowContext(ctx, query, date, hash).Scan(&exists)
	return exists, err
}

func (r *opsRepository) ClaimMonthlyUsageEmail(ctx context.Context, month, recipient string) (bool, error) {
	date, hash, err := monthlyDeliveryKey(month, recipient)
	if err != nil {
		return false, err
	}
	if r == nil || r.db == nil {
		return false, errors.New("missing monthly report database")
	}
	res, err := r.db.ExecContext(ctx, `
INSERT INTO monthly_usage_email_deliveries (report_month, recipient_hash)
VALUES ($1, $2) ON CONFLICT (report_month, recipient_hash) DO NOTHING`, date, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishMonthlyUsageEmail records SMTP acceptance or releases the claim after
// a definite failure. Callers must leave the claim alone for an unknown result.
func (r *opsRepository) FinishMonthlyUsageEmail(ctx context.Context, month, recipient string, delivered bool) error {
	date, hash, err := monthlyDeliveryKey(month, recipient)
	if err != nil {
		return err
	}
	if r == nil || r.db == nil {
		return errors.New("missing monthly report database")
	}
	if delivered {
		res, err := r.db.ExecContext(ctx, `
UPDATE monthly_usage_email_deliveries SET accepted_at = COALESCE(accepted_at, now())
WHERE report_month = $1 AND recipient_hash = $2`, date, hash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return errors.New("monthly usage email claim missing")
		}
		return err
	}
	_, err = r.db.ExecContext(ctx, `
DELETE FROM monthly_usage_email_deliveries
WHERE report_month = $1 AND recipient_hash = $2 AND accepted_at IS NULL`, date, hash)
	return err
}
