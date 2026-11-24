//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestModerationRollingWeekOpenAIOnly(t *testing.T) {
	ctx := context.Background()
	repo := &contentModerationRepository{db: integrationDB}
	settings := &moderationMailFencingSettings{values: map[string]string{
		service.SettingKeyRiskControlEnabled:      "true",
		service.SettingKeyContentModerationConfig: `{"enabled":true,"mode":"pre_block","keyword_blocking_mode":"keyword_only","blocked_keywords":["weekly-fixture"],"auto_ban_enabled":true,"ban_threshold":5,"violation_window_hours":168,"ban_scope":"openai","ban_duration_hours":24}`,
	}}
	svc := service.NewContentModerationService(settings, repo, nil, nil, nil, nil, nil, nil)
	var uid int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO users(email,password_hash) VALUES ($1,'fixture') RETURNING id`, fmt.Sprintf("weekly-%d@example.test", time.Now().UnixNano())).Scan(&uid))
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM content_moderation_logs WHERE user_id=$1`, uid)
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id=$1`, uid)
	})
	// Simulate an earlier counted request just outside the rolling window.
	_, err := integrationDB.Exec(`INSERT INTO content_moderation_user_hits(user_id,generation,request_id,action,created_at)
		VALUES ($1,0,'outside-window','keyword_block',now()-interval '168 hours 1 minute')`, uid)
	require.NoError(t, err)

	send := func(seq int, progress int64) {
		t.Helper()
		decision, err := svc.Check(ctx, service.ContentModerationCheckInput{
			UserID: uid, RequestID: fmt.Sprintf("weekly-%d", seq), Model: "fixture-model",
			Provider: service.PlatformOpenAI, Protocol: service.ContentModerationProtocolOpenAIChat,
			Body: []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"weekly-fixture %d"}]}`, seq)),
		})
		require.NoError(t, err)
		require.True(t, decision.Blocked)
		require.Eventually(t, func() bool {
			status, err := svc.GetStatus(ctx)
			return err == nil && status.Processed >= progress
		}, 10*time.Second, 10*time.Millisecond)
	}
	records := func() *service.MyModerationRecordsPage {
		t.Helper()
		page, err := svc.MyModerationRecords(ctx, uid, 1, 10)
		require.NoError(t, err)
		return page
	}
	for seq := 1; seq <= 4; seq++ {
		send(seq, int64(seq))
		if seq == 1 {
			_, err = integrationDB.Exec(`UPDATE content_moderation_user_hits SET created_at=now()-interval '167 hours 59 minutes' WHERE user_id=$1 AND request_id='weekly-1'`, uid)
			require.NoError(t, err)
		}
		require.NoError(t, svc.CheckPlatformAccess(ctx, uid, service.PlatformOpenAI))
		require.Equal(t, seq, records().CurrentCount)
	}
	send(4, 5) // Same request ID retried. It must not be the fifth hit.
	require.Equal(t, 4, records().CurrentCount)
	require.NoError(t, svc.CheckPlatformAccess(ctx, uid, service.PlatformOpenAI))
	send(5, 6)
	page := records()
	require.True(t, page.AutoBanEnabled)
	require.Equal(t, 5, page.Threshold)
	require.Equal(t, 5, page.CurrentCount)
	require.True(t, page.OpenAIBanned)
	require.False(t, page.AccountBanned)
	require.EqualValues(t, 1, page.RecordedBanCount)
	require.Len(t, page.Items, 5)
	latest := page.Items[0]
	require.Equal(t, "fixture-model", latest.Model)
	require.Contains(t, latest.InputExcerpt, "weekly-fixture 5")
	require.NotNil(t, latest.BanApplied)
	require.True(t, *latest.BanApplied)
	require.NotNil(t, latest.BanExpiresAt)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), *latest.BanExpiresAt, time.Minute)
	require.Equal(t, "openai_access_restricted", infraerrors.Reason(svc.CheckPlatformAccess(ctx, uid, service.PlatformOpenAI)))
	require.NoError(t, svc.CheckPlatformAccess(ctx, uid, service.PlatformDeepseek))
	_, err = integrationDB.Exec(`UPDATE content_moderation_user_state SET openai_ban_expires_at=NOW() WHERE user_id=$1`, uid)
	require.NoError(t, err)
	ids, err := repo.ExpireModerationBans(ctx, 64)
	require.NoError(t, err)
	require.Contains(t, ids, uid)
	require.NoError(t, svc.CheckPlatformAccess(ctx, uid, service.PlatformOpenAI))
	released := records()
	require.Zero(t, released.CurrentCount)
	require.False(t, released.OpenAIBanned)
	require.Len(t, released.Items, 5)
	require.EqualValues(t, 1, released.RecordedBanCount)
	send(6, 7)
	require.Equal(t, 1, records().CurrentCount)
	require.NoError(t, svc.CheckPlatformAccess(ctx, uid, service.PlatformOpenAI))
}
