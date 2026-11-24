package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Summary, total and page describe a single committed view, including when a
// ban is lifted or logs are pruned concurrently.
func (r *contentModerationRepository) MyModerationRecords(ctx context.Context, userID int64, page, size int, since time.Time, excludeCyber bool) (*service.MyModerationRecordsPage, error) {
	if userID <= 0 {
		return nil, service.ErrUserNotFound
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := scanModerationUser(tx.QueryRowContext(ctx, moderationUserProjection+`WHERE u.id=$3 AND u.deleted_at IS NULL`, since, excludeCyber, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	out := &service.MyModerationRecordsPage{Items: []service.MyModerationRecord{}, Page: page, PageSize: size, CurrentCount: state.CurrentCount, AccountBanned: state.AllPlatformsBlocked || state.OwnedDisable, OpenAIBanned: state.OpenAIBlocked, AutoBanEnabled: !state.AutoBanExempt}
	const countedLogs = ` FROM content_moderation_logs WHERE user_id=$1 AND flagged AND violation_count>0`
	var role string
	if err = tx.QueryRowContext(ctx, `SELECT u.role,COALESCE(s.recorded_ban_count,0),(SELECT COUNT(*)`+countedLogs+`)
		FROM users u LEFT JOIN content_moderation_user_state s ON s.user_id=u.id WHERE u.id=$1`, userID).Scan(&role, &out.RecordedBanCount, &out.Total); err != nil {
		return nil, err
	}
	out.AutoBanEnabled = out.AutoBanEnabled && role != service.RoleAdmin
	rows, err := tx.QueryContext(ctx, `SELECT id,created_at,model,highest_category,action,input_excerpt,violation_count,ban_applied,ban_scope,ban_expires_at`+countedLogs+` ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`, userID, size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item service.MyModerationRecord
		if err = rows.Scan(&item.ID, &item.CreatedAt, &item.Model, &item.Category, &item.Action, &item.InputExcerpt, &item.ViolationCount, &item.BanApplied, &item.BanScope, &item.BanExpiresAt); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
