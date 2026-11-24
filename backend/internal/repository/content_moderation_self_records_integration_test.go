//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMyModerationRecordsScopeAndHistory(t *testing.T) {
	ctx := context.Background()
	r := &contentModerationRepository{db: integrationDB}
	newOwner := func() int64 {
		t.Helper()
		var id int64
		require.NoError(t, integrationDB.QueryRow(`INSERT INTO users(email,password_hash,status) VALUES($1,'fixture','active') RETURNING id`,
			fmt.Sprintf("my-records-%d@example.test", time.Now().UnixNano())).Scan(&id))
		t.Cleanup(func() {
			_, _ = integrationDB.Exec(`DELETE FROM content_moderation_logs WHERE user_id=$1`, id)
			_, _ = integrationDB.Exec(`DELETE FROM users WHERE id=$1`, id)
		})
		return id
	}
	owner, other := newOwner(), newOwner()
	since := time.Now().Add(-time.Hour)
	read := func(id int64, page, size int) *service.MyModerationRecordsPage {
		t.Helper()
		p, err := r.MyModerationRecords(ctx, id, page, size, since, false)
		require.NoError(t, err)
		return p
	}
	require.Empty(t, read(other, 1, 10).Items)
	otherLog := service.ContentModerationLog{UserID: &other, Flagged: true, ViolationCount: 5, InputExcerpt: "other user's private excerpt"}
	require.NoError(t, r.CreateLog(ctx, &otherLog))
	// Historical status is not evidence of which request imposed the ban.
	legacy := service.ContentModerationLog{UserID: &owner, Flagged: true, ViolationCount: 7, AutoBanned: true, InputExcerpt: "retained legacy excerpt"}
	require.NoError(t, r.CreateLog(ctx, &legacy))
	for i := 1; i <= 3; i++ {
		version := int64(0)
		if i == 3 {
			version = 1
		}
		result, err := r.ApplyModerationHit(ctx, service.ModerationHit{UserID: owner, Version: version, RequestID: fmt.Sprint(i), Since: since,
			Action: "keyword_block", BanScope: "openai", AutoBan: true, BanDurationHours: 2, Threshold: 2})
		require.NoError(t, err)
		require.True(t, result.Accepted)
		log := service.ContentModerationLog{UserID: &owner, Flagged: true, ViolationCount: result.Count, AutoBanned: result.OpenAIBlocked,
			BanApplied: &result.BanApplied, BanExpiresAt: result.BanExpiresAt, BanScope: "openai", InputExcerpt: fmt.Sprintf("my excerpt %d", i)}
		require.NoError(t, r.CreateLog(ctx, &log))
	}
	first := read(owner, 1, 2)
	require.EqualValues(t, 4, first.Total)
	require.Equal(t, 3, first.CurrentCount)
	require.True(t, first.OpenAIBanned)
	require.False(t, first.AccountBanned)
	require.EqualValues(t, 1, first.RecordedBanCount)
	require.Len(t, first.Items, 2)
	require.Equal(t, "my excerpt 3", first.Items[0].InputExcerpt)
	require.NotNil(t, first.Items[0].BanApplied)
	require.False(t, *first.Items[0].BanApplied)
	require.True(t, *first.Items[1].BanApplied)
	require.NotNil(t, first.Items[1].BanExpiresAt)
	second := read(owner, 2, 2)
	require.Len(t, second.Items, 2)
	require.Nil(t, second.Items[1].BanApplied)
	require.Equal(t, "retained legacy excerpt", second.Items[1].InputExcerpt)
	_, err := integrationDB.Exec(`UPDATE content_moderation_user_state SET openai_ban_expires_at=NOW() WHERE user_id=$1`, owner)
	require.NoError(t, err)
	_, err = r.ExpireModerationBans(ctx, 64)
	require.NoError(t, err)
	after := read(owner, 1, 10)
	require.Zero(t, after.CurrentCount)
	require.False(t, after.OpenAIBanned)
	require.EqualValues(t, 1, after.RecordedBanCount)
	require.Len(t, after.Items, 4)
	_, err = integrationDB.Exec(`DELETE FROM content_moderation_logs WHERE user_id=$1`, owner)
	require.NoError(t, err)
	after = read(owner, 1, 10)
	require.Empty(t, after.Items)
	require.EqualValues(t, 1, after.RecordedBanCount)
	require.Equal(t, "other user's private excerpt", read(other, 1, 10).Items[0].InputExcerpt)
}
