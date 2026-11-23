package repository

import (
	"context"
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Compare the original database value in the write statement so concurrent
// editors on different instances cannot both save the same revision.
func (r *settingRepository) CompareAndSetOpsEmailConfig(ctx context.Context, previous *string, next string) error {
	var res sql.Result
	var err error
	if previous == nil {
		res, err = r.client.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at)
VALUES ($1, $2, now()) ON CONFLICT (key) DO NOTHING`, service.SettingKeyOpsEmailNotificationConfig, next)
	} else {
		res, err = r.client.ExecContext(ctx, `UPDATE settings SET value = $2, updated_at = now()
WHERE key = $1 AND value = $3`, service.SettingKeyOpsEmailNotificationConfig, next, *previous)
	}
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err == nil && count != 1 {
		return service.ErrReportRevisionConflict
	}
	return err
}
