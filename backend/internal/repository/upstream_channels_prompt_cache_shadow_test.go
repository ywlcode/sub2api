//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newUpstreamChannelShadowMock(t *testing.T) (*channelRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return &channelRepository{db: db}, mock
}

func upstreamChannelShadowRow(enabled bool, revision int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "code", "name", "remark", "revision", "members", "prompt_cache_shadow_enabled"}).
		AddRow(7, "shadow", "Shadow", "observation", revision, 2, enabled)
}

func TestUpstreamChannelPromptCacheShadowCreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *bool
		want  bool
	}{
		{name: "omitted defaults false"},
		{name: "explicit false", value: new(bool)},
		{name: "explicit true", value: func() *bool { v := true; return &v }(), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := newUpstreamChannelShadowMock(t)
			mock.ExpectQuery(`INSERT INTO upstream_channels\(code,name,remark,prompt_cache_shadow_enabled\)`).
				WithArgs("shadow", "Shadow", "observation", tc.want).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
			mock.ExpectQuery(`SELECT .*c.prompt_cache_shadow_enabled.* FROM upstream_channels c WHERE c.id=\$1`).
				WithArgs(int64(7)).WillReturnRows(upstreamChannelShadowRow(tc.want, 1))
			channel, err := repo.CreateUpstreamChannel(context.Background(), service.UpstreamChannelMutation{
				Code: "shadow", Name: "Shadow", Remark: "observation", Confirm: true, PromptCacheShadowEnabled: tc.value,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, channel.PromptCacheShadowEnabled)
			require.EqualValues(t, 1, channel.Revision)
		})
	}
}

func TestUpstreamChannelPromptCacheShadowUpdateCAS(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *bool
		want  bool
	}{
		{name: "omitted preserves enabled", want: true},
		{name: "omitted preserves disabled"},
		{name: "explicit false", value: new(bool)},
		{name: "explicit true", value: func() *bool { v := true; return &v }(), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := newUpstreamChannelShadowMock(t)
			mock.ExpectQuery(`UPDATE upstream_channels SET .*prompt_cache_shadow_enabled=COALESCE\(\$6,prompt_cache_shadow_enabled\).* WHERE id=\$1 AND revision=\$4 AND \(\$5='' OR code=\$5\) RETURNING id`).
				WithArgs(int64(7), "Shadow", "observation", int64(4), "shadow", tc.value).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
			mock.ExpectQuery(`SELECT .* FROM upstream_channels c WHERE c.id=\$1`).
				WithArgs(int64(7)).WillReturnRows(upstreamChannelShadowRow(tc.want, 5))
			revision := int64(4)
			channel, err := repo.UpdateUpstreamChannel(context.Background(), 7, service.UpstreamChannelMutation{
				Code: "shadow", Name: "Shadow", Remark: "observation", Revision: &revision, Confirm: true, PromptCacheShadowEnabled: tc.value,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, channel.PromptCacheShadowEnabled)
			require.EqualValues(t, 5, channel.Revision)
		})
	}
	t.Run("stale revision is HTTP 409", func(t *testing.T) {
		repo, mock := newUpstreamChannelShadowMock(t)
		mock.ExpectQuery(`UPDATE upstream_channels SET .* WHERE id=\$1 AND revision=\$4`).
			WithArgs(int64(7), "Shadow", "", int64(3), "", false).WillReturnError(sql.ErrNoRows)
		revision := int64(3)
		_, err := repo.UpdateUpstreamChannel(context.Background(), 7, service.UpstreamChannelMutation{
			Name: "Shadow", Revision: &revision, Confirm: true, PromptCacheShadowEnabled: new(bool),
		})
		require.ErrorIs(t, err, service.ErrUpstreamChannelConflict)
		status, _ := infraerrors.ToHTTP(err)
		require.Equal(t, http.StatusConflict, status)
	})
	t.Run("confirmation required before SQL", func(t *testing.T) {
		repo, _ := newUpstreamChannelShadowMock(t)
		revision := int64(3)
		_, err := repo.UpdateUpstreamChannel(context.Background(), 7, service.UpstreamChannelMutation{
			Name: "Shadow", Revision: &revision, PromptCacheShadowEnabled: new(bool),
		})
		require.ErrorIs(t, err, service.ErrUpstreamChannelValidation)
	})
}

func TestUpstreamChannelPromptCacheShadowListScan(t *testing.T) {
	repo, mock := newUpstreamChannelShadowMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT count\(\*\) FROM upstream_channels`).WithArgs("Shadow").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT .*c.prompt_cache_shadow_enabled.* FROM upstream_channels c .*ORDER BY c.id LIMIT \$2 OFFSET \$3`).
		WithArgs("Shadow", 10, 0).WillReturnRows(upstreamChannelShadowRow(true, 4)).RowsWillBeClosed()
	mock.ExpectCommit()
	page, err := repo.ListUpstreamChannels(context.Background(), "Shadow", 1, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].PromptCacheShadowEnabled)
	require.EqualValues(t, 2, page.Items[0].Members)
}

func TestUpstreamChannelPromptCacheShadowBindingScan(t *testing.T) {
	repo, mock := newUpstreamChannelShadowMock(t)
	mock.ExpectQuery(`SELECT .*COALESCE\(c.prompt_cache_shadow_enabled,false\).* FROM accounts a LEFT JOIN upstream_channel_bindings`).
		WithArgs("[10,11,12]").WillReturnRows(sqlmock.NewRows([]string{
		"account_id", "account_name", "deleted", "channel_id", "revision", "code", "name", "remark", "channel_revision", "prompt_cache_shadow_enabled",
	}).AddRow(10, "enabled", false, 7, 2, "shadow", "Shadow", "", 4, true).
		AddRow(11, "disabled", false, 8, 3, "other", "Other", "", 5, false).
		AddRow(12, "unbound", false, 0, 4, "", "", "", 0, false)).RowsWillBeClosed()
	bindings, err := repo.ReadUpstreamChannelBindings(context.Background(), []int64{10, 11, 12})
	require.NoError(t, err)
	require.Len(t, bindings, 3)
	require.NotNil(t, bindings[0].Channel)
	require.True(t, bindings[0].Channel.PromptCacheShadowEnabled)
	require.False(t, bindings[1].Channel.PromptCacheShadowEnabled)
	require.Nil(t, bindings[2].Channel)
}

func TestUpstreamChannelPromptCacheShadowAccounts(t *testing.T) {
	const query = `SELECT b.account_id,b.channel_id FROM upstream_channel_bindings b JOIN upstream_channels c ON c.id=b.channel_id JOIN accounts a ON a.id=b.account_id WHERE c.prompt_cache_shadow_enabled AND a.deleted_at IS NULL AND a.platform=\$1 ORDER BY b.account_id`
	t.Run("one bulk query", func(t *testing.T) {
		repo, mock := newUpstreamChannelShadowMock(t)
		shadow, ok := any(repo).(service.UpstreamChannelPromptCacheShadowRepository)
		require.True(t, ok, "channel repository must support the service snapshot loader")
		mock.ExpectQuery(query).WithArgs(service.PlatformOpenAI).
			WillReturnRows(sqlmock.NewRows([]string{"account_id", "channel_id"}).AddRow(10, 7).AddRow(11, 8)).RowsWillBeClosed()
		accounts, err := shadow.ListPromptCacheShadowAccounts(context.Background())
		require.NoError(t, err)
		require.Equal(t, []service.UpstreamChannelPromptCacheShadowAccount{{AccountID: 10, ChannelID: 7}, {AccountID: 11, ChannelID: 8}}, accounts)
	})
	for _, mode := range []string{"empty", "query error", "scan error", "iteration error"} {
		t.Run(mode, func(t *testing.T) {
			repo, mock := newUpstreamChannelShadowMock(t)
			shadow, ok := any(repo).(service.UpstreamChannelPromptCacheShadowRepository)
			require.True(t, ok)
			wantErr := errors.New("snapshot unavailable")
			expect := mock.ExpectQuery(query).WithArgs(service.PlatformOpenAI)
			rows := sqlmock.NewRows([]string{"account_id", "channel_id"})
			switch mode {
			case "query error":
				expect.WillReturnError(wantErr)
			case "scan error":
				expect.WillReturnRows(rows.AddRow("bad-id", 7)).RowsWillBeClosed()
			case "iteration error":
				expect.WillReturnRows(rows.AddRow(10, 7).AddRow(11, 7).RowError(1, wantErr)).RowsWillBeClosed()
			default:
				expect.WillReturnRows(rows).RowsWillBeClosed()
			}
			accounts, err := shadow.ListPromptCacheShadowAccounts(context.Background())
			if mode == "empty" {
				require.NoError(t, err)
				require.NotNil(t, accounts)
				require.Empty(t, accounts)
			} else if mode == "scan error" {
				require.Error(t, err)
			} else {
				require.ErrorIs(t, err, wantErr)
			}
		})
	}
}
