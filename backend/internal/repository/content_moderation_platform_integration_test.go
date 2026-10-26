//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestModerationPlatformBanLifecycleAndConcurrentRecovery(t *testing.T) {
	ctx := context.Background()
	r := &contentModerationRepository{db: integrationDB}
	for _, globalDisable := range []bool{false, true} {
		t.Run(fmt.Sprintf("other_source_disable_%t", globalDisable), func(t *testing.T) {
			var id int64
			require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,status) VALUES($1,'fixture','active') RETURNING id`, fmt.Sprintf("platform-%d@example.test", time.Now().UnixNano())).Scan(&id))
			t.Cleanup(func() {
				_, _ = integrationDB.Exec(`DELETE FROM content_moderation_user_actions WHERE user_id=$1`, id)
				_, _ = integrationDB.Exec(`DELETE FROM users WHERE id=$1`, id)
			})
			hit := service.ModerationHit{UserID: id, Version: 0, RequestID: "same-request", Action: "keyword_block", Since: time.Now().Add(-time.Hour), AutoBan: true, BanScope: service.ContentModerationBanScopeOpenAI, Threshold: 1}
			var wg sync.WaitGroup
			errCh := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := r.ApplyModerationHit(ctx, hit); errCh <- err }()
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				require.NoError(t, err)
			}
			state, err := r.GetModerationUser(ctx, id, hit.Since, false)
			require.NoError(t, err)
			require.True(t, state.OpenAIBlocked)
			require.False(t, state.OwnedDisable)
			require.Equal(t, service.StatusActive, state.Status, "platform ban must preserve login and other platforms")
			require.Equal(t, 1, state.CurrentCount)
			require.EqualValues(t, 1, state.Version)
			blocked, err := r.IsOpenAIBlocked(ctx, id)
			require.NoError(t, err)
			require.True(t, blocked)
			// Reset and exemption changes must preserve the already-issued ban.
			for _, action := range []string{"reset", "exemption"} {
				in := service.ModerationUserAction{Action: action, Version: state.Version, ActorID: id, Reason: "fixture", Confirm: true}
				if action == "exemption" {
					yes := true
					in.Exempt = &yes
				}
				require.NoError(t, r.ManageModerationUser(ctx, id, in))
				state, err = r.GetModerationUser(ctx, id, hit.Since, false)
				require.NoError(t, err)
				require.True(t, state.OpenAIBlocked)
			}
			if globalDisable {
				_, err = integrationDB.ExecContext(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, id)
				require.NoError(t, err)
			}
			require.NoError(t, r.ManageModerationUser(ctx, id, service.ModerationUserAction{Action: "unban", Version: state.Version, ActorID: id, Reason: "reviewed", Confirm: true}))
			state, err = r.GetModerationUser(ctx, id, hit.Since, false)
			require.NoError(t, err)
			require.False(t, state.OpenAIBlocked)
			wantStatus := service.StatusActive
			if globalDisable {
				wantStatus = service.StatusDisabled
			}
			require.Equal(t, wantStatus, state.Status, "release must not undo another source's disable")
			stale, err := r.ApplyModerationHit(ctx, hit)
			require.NoError(t, err)
			require.False(t, stale.Accepted)
			blocked, err = r.IsOpenAIBlocked(ctx, id)
			require.NoError(t, err)
			require.False(t, blocked, "an old asynchronous result cannot reban after release")
		})
	}
}

func TestModerationPlatformBanMigrationPreservesLegacyState(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	_, err := tx.ExecContext(ctx, `ALTER TABLE content_moderation_user_state DROP COLUMN openai_blocked; ALTER TABLE content_moderation_logs DROP COLUMN ban_scope;`)
	require.NoError(t, err)
	var id int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,status) VALUES('legacy-platform@example.test','fixture','disabled') RETURNING id`).Scan(&id))
	_, err = tx.ExecContext(ctx, `INSERT INTO content_moderation_user_state(user_id,version,generation) VALUES($1,7,2)`, id)
	require.NoError(t, err)
	migration, err := dbmigrations.FS.ReadFile("307_content_moderation_platform_bans.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	var blocked bool
	var status string
	var version int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT s.openai_blocked,s.version,u.status FROM content_moderation_user_state s JOIN users u ON u.id=s.user_id WHERE u.id=$1`, id).Scan(&blocked, &version, &status))
	require.False(t, blocked)
	require.EqualValues(t, 7, version)
	require.Equal(t, "disabled", status)
	_, err = tx.ExecContext(ctx, `UPDATE content_moderation_user_state SET openai_blocked=TRUE WHERE user_id=$1`, id)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT openai_blocked FROM content_moderation_user_state WHERE user_id=$1`, id).Scan(&blocked))
	require.True(t, blocked, "migration replay must retain existing restrictions")
}
