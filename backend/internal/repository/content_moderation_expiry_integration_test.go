//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestModerationBanExpiry(t *testing.T) {
	ctx := context.Background()
	r := &contentModerationRepository{db: integrationDB}
	newUser := func(t *testing.T) int64 {
		t.Helper()
		var id int64
		require.NoError(t, integrationDB.QueryRow(`INSERT INTO users(email,password_hash,status)
			VALUES ($1,'fixture','active') RETURNING id`, fmt.Sprintf("expire-%d@example.test", time.Now().UnixNano())).Scan(&id))
		t.Cleanup(func() {
			_, _ = integrationDB.Exec(`DELETE FROM content_moderation_user_actions WHERE user_id=$1`, id)
			_, _ = integrationDB.Exec(`DELETE FROM users WHERE id=$1`, id)
		})
		return id
	}
	ban := func(t *testing.T, id, version int64, scope string) time.Time {
		t.Helper()
		result, err := r.ApplyModerationHit(ctx, service.ModerationHit{UserID: id, Version: version, RequestID: fmt.Sprintf("%s-%d", scope, version),
			Action: "keyword_block", Since: time.Now().Add(-time.Hour), AutoBan: true, BanScope: scope, BanDurationHours: 7, Threshold: 1})
		require.NoError(t, err)
		require.True(t, result.BanApplied)
		require.NotNil(t, result.BanExpiresAt)
		require.WithinDuration(t, time.Now().Add(7*time.Hour), *result.BanExpiresAt, time.Minute)
		return *result.BanExpiresAt
	}
	state := func(t *testing.T, id int64) *service.ModerationUserState {
		t.Helper()
		s, err := r.GetModerationUser(ctx, id, time.Now().Add(-time.Hour), false)
		require.NoError(t, err)
		return s
	}
	t.Run("overlapping bans and concurrent workers", func(t *testing.T) {
		id := newUser(t)
		openai := ban(t, id, 0, "openai")
		account := ban(t, id, 1, "all")
		var a, b time.Time
		require.NoError(t, integrationDB.QueryRow(`SELECT account_ban_expires_at, openai_ban_expires_at FROM content_moderation_user_state WHERE user_id=$1`, id).Scan(&a, &b))
		require.True(t, account.Equal(a))
		require.True(t, openai.Equal(b))
		ids, err := r.ExpireModerationBans(ctx, 64)
		require.NoError(t, err)
		require.NotContains(t, ids, id)
		_, err = integrationDB.Exec(`UPDATE content_moderation_user_state SET account_ban_expires_at=NOW() WHERE user_id=$1`, id)
		require.NoError(t, err)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := r.ExpireModerationBans(ctx, 64)
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		s := state(t, id)
		require.Equal(t, "active", s.Status)
		require.True(t, s.OpenAIBlocked)
		require.EqualValues(t, 3, s.Version)
		require.Zero(t, s.CurrentCount)
		stale, err := r.ApplyModerationHit(ctx, service.ModerationHit{UserID: id, Version: 2, RequestID: "queued", Since: time.Now(), AutoBan: true, Threshold: 1})
		require.NoError(t, err)
		require.False(t, stale.Accepted)
		_, err = integrationDB.Exec(`UPDATE content_moderation_user_state SET openai_ban_expires_at=NOW() WHERE user_id=$1`, id)
		require.NoError(t, err)
		blocked, err := r.IsOpenAIBlocked(ctx, id)
		require.NoError(t, err)
		require.True(t, blocked, "admission and count reset must take effect at the same commit")
		ids, err = r.ExpireModerationBans(ctx, 64)
		require.NoError(t, err)
		require.Contains(t, ids, id)
		require.EqualValues(t, 4, state(t, id).Version)
		blocked, err = r.IsOpenAIBlocked(ctx, id)
		require.NoError(t, err)
		require.False(t, blocked)
	})
	t.Run("manual disable takes ownership", func(t *testing.T) {
		id := newUser(t)
		ban(t, id, 0, "all")
		_, err := integrationDB.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, id)
		require.NoError(t, err)
		_, err = integrationDB.Exec(`UPDATE content_moderation_user_state SET account_ban_expires_at=NOW() WHERE user_id=$1`, id)
		require.NoError(t, err)
		_, err = r.ExpireModerationBans(ctx, 64)
		require.NoError(t, err)
		s := state(t, id)
		require.Equal(t, "disabled", s.Status)
		require.False(t, s.OwnedDisable)
	})
	for _, scope := range []string{"all", "openai"} {
		t.Run("new violations start at one after "+scope+" expiry", func(t *testing.T) {
			id := newUser(t)
			record := func(version int64, request string) service.ModerationHitResult {
				t.Helper()
				hit, err := r.ApplyModerationHit(ctx, service.ModerationHit{UserID: id, Version: version, RequestID: request,
					Action: "keyword_block", Since: time.Now().Add(-time.Hour), AutoBan: true, BanScope: scope, Threshold: 2, BanDurationHours: 1})
				require.NoError(t, err)
				return hit
			}
			require.False(t, record(0, "before-1").BanApplied)
			require.True(t, record(0, "before-2").BanApplied)
			previous := state(t, id)
			require.Equal(t, 2, previous.CurrentCount)
			_, err := integrationDB.Exec(`UPDATE content_moderation_user_state SET
				account_ban_expires_at=CASE WHEN all_platforms_blocked THEN NOW() END,
				openai_ban_expires_at=CASE WHEN openai_blocked THEN NOW() END WHERE user_id=$1`, id)
			require.NoError(t, err)
			_, err = r.ExpireModerationBans(ctx, 64)
			require.NoError(t, err)
			released := state(t, id)
			require.Zero(t, released.CurrentCount)
			require.False(t, record(previous.Version, "queued-before-release").Accepted)
			first := record(released.Version, "after-1")
			require.Equal(t, 1, first.Count)
			require.False(t, first.BanApplied)
			require.True(t, record(released.Version, "after-2").BanApplied)
			var history int
			require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM content_moderation_user_hits WHERE user_id=$1 AND generation=$2`, id, previous.Generation).Scan(&history))
			require.Equal(t, 2, history)
		})
	}
	for _, scope := range []string{"all", "openai"} {
		t.Run("early release followed by a new "+scope+" ban", func(t *testing.T) {
			id := newUser(t)
			ban(t, id, 0, scope)
			// The old deadline is due; an administrator releases the user before
			// the worker claims it. A later offence has its own full duration.
			_, err := integrationDB.Exec(`UPDATE content_moderation_user_state SET
				account_ban_expires_at=CASE WHEN all_platforms_blocked THEN NOW() END,
				openai_ban_expires_at=CASE WHEN openai_blocked THEN NOW() END WHERE user_id=$1`, id)
			require.NoError(t, err)
			require.NoError(t, r.ManageModerationUser(ctx, id, service.ModerationUserAction{Action: "unban", Version: 1, Reason: "early release", ActorID: id}))
			released := state(t, id)
			require.Equal(t, "active", released.Status)
			require.False(t, released.OpenAIBlocked)
			require.False(t, released.AllPlatformsBlocked)
			ban(t, id, released.Version, scope)
			ids, err := r.ExpireModerationBans(ctx, 64)
			require.NoError(t, err)
			require.NotContains(t, ids, id)
			current := state(t, id)
			require.Equal(t, scope == "all", current.AllPlatformsBlocked)
			require.Equal(t, scope == "openai", current.OpenAIBlocked)
		})
	}
	t.Run("early unban and legacy bans", func(t *testing.T) {
		id := newUser(t)
		ban(t, id, 0, "openai")
		ban(t, id, 1, "all")
		err := r.ManageModerationUser(ctx, id, service.ModerationUserAction{Action: "unban", Version: 2, Reason: "reviewed", ActorID: id})
		require.NoError(t, err)
		var account, platform sql.NullTime
		require.NoError(t, integrationDB.QueryRow(`SELECT account_ban_expires_at,openai_ban_expires_at FROM content_moderation_user_state WHERE user_id=$1`, id).Scan(&account, &platform))
		require.False(t, account.Valid)
		require.False(t, platform.Valid)
		// Simulate pre-upgrade writes. NULL deadlines must not grant access.
		_, err = integrationDB.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, id)
		require.NoError(t, err)
		_, err = integrationDB.Exec(`UPDATE content_moderation_user_state SET openai_blocked=TRUE,
			owned_status_revision=(SELECT moderation_status_revision FROM users WHERE id=$1) WHERE user_id=$1`, id)
		require.NoError(t, err)
		ids, err := r.ExpireModerationBans(ctx, 64)
		require.NoError(t, err)
		require.NotContains(t, ids, id)
		s := state(t, id)
		require.True(t, s.OwnedDisable)
		require.True(t, s.OpenAIBlocked)
	})
}
