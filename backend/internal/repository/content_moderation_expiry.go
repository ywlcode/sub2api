package repository

import (
	"context"

	"github.com/lib/pq"
)

// ExpireModerationBans uses the same user-first lock order as moderation hits
// and administrator actions. A second worker can process other due users.
func (r *contentModerationRepository) ExpireModerationBans(ctx context.Context, limit int) ([]int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var ids pq.Int64Array
	err = tx.QueryRowContext(ctx, `SELECT ARRAY(
		SELECT u.id FROM content_moderation_user_state s JOIN users u ON u.id=s.user_id
		WHERE LEAST(s.account_ban_expires_at,s.openai_ban_expires_at) <= NOW()
		ORDER BY LEAST(s.account_ban_expires_at,s.openai_ban_expires_at),s.user_id
		LIMIT $1 FOR UPDATE OF u SKIP LOCKED)`, limit).Scan(&ids)
	if err != nil || len(ids) == 0 {
		return nil, err
	}

	// Recheck after acquiring the user locks. An explicit admin disable, even
	// a second write of 'disabled', advances the revision and takes ownership.
	if _, err = tx.ExecContext(ctx, `UPDATE users u SET status='active',updated_at=NOW()
		FROM content_moderation_user_state s WHERE u.id=s.user_id AND u.id=ANY($1)
		AND u.deleted_at IS NULL AND u.status='disabled'
		AND u.moderation_status_revision=s.owned_status_revision AND s.account_ban_expires_at <= NOW()`, ids); err != nil {
		return nil, err
	}
	// Start a fresh hit window and fence results queued before expiry. Preserve
	// any other restriction (including one without a deadline).
	if _, err = tx.ExecContext(ctx, `UPDATE content_moderation_user_state SET
		all_platforms_blocked=CASE WHEN account_ban_expires_at <= NOW() THEN FALSE ELSE all_platforms_blocked END,
		owned_status_revision=CASE WHEN account_ban_expires_at <= NOW() THEN NULL ELSE owned_status_revision END,
		account_ban_expires_at=CASE WHEN account_ban_expires_at <= NOW() THEN NULL ELSE account_ban_expires_at END,
		openai_blocked=CASE WHEN openai_ban_expires_at <= NOW() THEN FALSE ELSE openai_blocked END,
		openai_ban_expires_at=CASE WHEN openai_ban_expires_at <= NOW() THEN NULL ELSE openai_ban_expires_at END,
		version=version+1,generation=generation+1,updated_at=NOW()
		WHERE user_id=ANY($1) AND LEAST(account_ban_expires_at,openai_ban_expires_at) <= NOW()`, ids); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}
