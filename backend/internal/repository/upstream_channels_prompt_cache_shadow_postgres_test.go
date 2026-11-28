package repository

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Opt-in fixture using session-local tables and the real channel migrations.
// No shared schema, application rows, Docker containers, or networks are changed.
func TestUpstreamChannelPromptCacheShadowPostgres(t *testing.T) {
	dsn := os.Getenv("UPSTREAM_CHANNEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("SUB2API_TEST_POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("set UPSTREAM_CHANNEL_TEST_POSTGRES_DSN to a disposable PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE accounts(
id BIGINT PRIMARY KEY, name TEXT NOT NULL, platform TEXT NOT NULL, type TEXT NOT NULL,
deleted_at TIMESTAMPTZ, status TEXT NOT NULL DEFAULT 'active', schedulable BOOLEAN NOT NULL DEFAULT true,
credentials JSONB NOT NULL DEFAULT '{"base_url":"https://shared.example/v1"}');
SET search_path TO pg_temp;
CREATE TEMP TABLE usage_logs(id BIGINT PRIMARY KEY, account_id BIGINT, input_tokens BIGINT, cache_read_tokens BIGINT, actual_cost NUMERIC);
INSERT INTO usage_logs VALUES(1,1,5000,4000,0.123456),(2,2,6000,3000,0.654321);
INSERT INTO accounts(id,name,platform,type) VALUES
(1,'openai-oauth','openai','oauth'),(2,'openai-key','openai','apikey'),
(3,'deleted','openai','oauth'),(4,'anthropic','anthropic','apikey'),
(5,'gemini','gemini','oauth'),(6,'deepseek','deepseek','apikey'),
(7,'unbound','openai','apikey'),(8,'tombstone','openai','oauth'),
(9,'disabled-channel','openai','apikey'),(10,'unschedulable','openai','oauth');
UPDATE accounts SET deleted_at=CURRENT_TIMESTAMP WHERE id=3;
UPDATE accounts SET status='disabled',schedulable=false WHERE id=10`)
	require.NoError(t, err)
	for _, filename := range []string{"312_upstream_channel_entities.sql", "396_upstream_channel_prompt_cache_shadow.sql"} {
		content, err := migrations.FS.ReadFile(filename)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, strings.ReplaceAll(string(content), "CREATE TABLE IF NOT EXISTS", "CREATE TEMP TABLE IF NOT EXISTS"))
		require.NoError(t, err)
	}
	repo := &channelRepository{db: db}
	shadow, ok := any(repo).(service.UpstreamChannelPromptCacheShadowRepository)
	require.True(t, ok)
	usageSnapshot := func() string {
		t.Helper()
		var snapshot string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM usage_logs u`).Scan(&snapshot))
		return snapshot
	}
	usageBefore := usageSnapshot()
	create := func(code string, enabled *bool) *service.UpstreamChannel {
		t.Helper()
		channel, err := repo.CreateUpstreamChannel(ctx, service.UpstreamChannelMutation{
			Code: code, Name: code, Confirm: true, PromptCacheShadowEnabled: enabled,
		})
		require.NoError(t, err)
		return channel
	}
	get := func(id int64) *service.UpstreamChannel {
		t.Helper()
		channel, err := repo.GetUpstreamChannel(ctx, id)
		require.NoError(t, err)
		return channel
	}
	update := func(channel *service.UpstreamChannel, enabled *bool) *service.UpstreamChannel {
		t.Helper()
		saved, err := repo.UpdateUpstreamChannel(ctx, channel.ID, service.UpstreamChannelMutation{
			Code: channel.Code, Name: channel.Name, Remark: "updated", Revision: &channel.Revision,
			Confirm: true, PromptCacheShadowEnabled: enabled,
		})
		require.NoError(t, err)
		require.Equal(t, channel.Revision+1, saved.Revision)
		return saved
	}
	assertConflict := func(err error) {
		t.Helper()
		require.ErrorIs(t, err, service.ErrUpstreamChannelConflict)
		status, _ := infraerrors.ToHTTP(err)
		require.Equal(t, http.StatusConflict, status)
	}
	enabled, disabled := true, false
	defaultChannel := create("default", nil)
	require.False(t, defaultChannel.PromptCacheShadowEnabled)
	require.EqualValues(t, 1, defaultChannel.Revision)
	defaultChannel = update(defaultChannel, nil)
	require.False(t, defaultChannel.PromptCacheShadowEnabled)
	a, b, off := create("enabled-a", &enabled), create("enabled-b", &enabled), create("disabled", &disabled)
	require.True(t, a.PromptCacheShadowEnabled)
	require.True(t, b.PromptCacheShadowEnabled)
	require.False(t, off.PromptCacheShadowEnabled)
	a = update(a, nil)
	require.True(t, a.PromptCacheShadowEnabled, "old PUT clients preserve enabled")
	stale := *a
	a = update(a, &disabled)
	require.False(t, a.PromptCacheShadowEnabled, "explicit false must not behave like omission")
	_, err = repo.UpdateUpstreamChannel(ctx, a.ID, service.UpstreamChannelMutation{
		Name: "stale-write", Revision: &stale.Revision, Confirm: true, PromptCacheShadowEnabled: &enabled,
	})
	assertConflict(err)
	require.Equal(t, a, get(a.ID), "stale writes change neither switch nor metadata")
	_, err = repo.UpdateUpstreamChannel(ctx, a.ID, service.UpstreamChannelMutation{
		Name: "unconfirmed", Revision: &a.Revision, PromptCacheShadowEnabled: &enabled,
	})
	require.ErrorIs(t, err, service.ErrUpstreamChannelValidation)
	a = update(a, nil)
	require.False(t, a.PromptCacheShadowEnabled, "omitted false also survives")
	a = update(a, &enabled)
	page, err := repo.ListUpstreamChannels(ctx, "", 1, 100)
	require.NoError(t, err)
	require.Len(t, page.Items, 4)
	for _, item := range page.Items {
		require.Equal(t, item.ID == a.ID || item.ID == b.ID, item.PromptCacheShadowEnabled)
	}
	bindings := func(ids ...int64) []service.UpstreamChannelBinding {
		t.Helper()
		out, err := repo.ReadUpstreamChannelBindings(ctx, ids)
		require.NoError(t, err)
		require.Len(t, out, len(ids))
		return out
	}
	bind := func(channelID int64, ids ...int64) []service.UpstreamChannelBinding {
		t.Helper()
		out, err := repo.BindUpstreamChannelAccounts(ctx, service.UpstreamChannelBindingsMutation{
			ChannelID: channelID, Bindings: bindings(ids...), Confirm: true,
		})
		require.NoError(t, err)
		for _, binding := range out {
			if channelID == 0 {
				require.Nil(t, binding.Channel)
			} else {
				require.NotNil(t, binding.Channel)
				require.Equal(t, get(channelID).PromptCacheShadowEnabled, binding.Channel.PromptCacheShadowEnabled)
			}
		}
		return out
	}
	assertAccounts := func(want ...service.UpstreamChannelPromptCacheShadowAccount) {
		t.Helper()
		got, err := shadow.ListPromptCacheShadowAccounts(ctx)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, append([]service.UpstreamChannelPromptCacheShadowAccount{}, want...), got)
	}
	account := func(id, channelID int64) service.UpstreamChannelPromptCacheShadowAccount {
		return service.UpstreamChannelPromptCacheShadowAccount{AccountID: id, ChannelID: channelID}
	}
	assertAccounts()
	// Deleted accounts retain analytics ownership, but are excluded from shadow
	// observation. Ordinary binding mutations correctly refuse deleted accounts.
	bind(a.ID, 1, 4, 5, 6, 10)
	_, err = db.ExecContext(ctx, `INSERT INTO upstream_channel_bindings(account_id,channel_id,revision) VALUES(3,$1,1)`, a.ID)
	require.NoError(t, err)
	bind(b.ID, 2)
	bind(off.ID, 9)
	bind(a.ID, 8)
	tombstone := bind(0, 8)[0]
	require.Positive(t, tombstone.Revision)
	assertAccounts(account(1, a.ID), account(2, b.ID), account(10, a.ID))
	require.True(t, bindings(3)[0].Deleted)
	require.True(t, bindings(3)[0].Channel.PromptCacheShadowEnabled)
	a = update(get(a.ID), &disabled)
	assertAccounts(account(2, b.ID))
	a = update(a, &enabled)
	assertAccounts(account(1, a.ID), account(2, b.ID), account(10, a.ID))
	oldBindings := bindings(1, 2)
	bind(b.ID, 1)
	assertAccounts(account(1, b.ID), account(2, b.ID), account(10, a.ID))
	_, err = repo.BindUpstreamChannelAccounts(ctx, service.UpstreamChannelBindingsMutation{
		ChannelID: off.ID, Bindings: oldBindings, Confirm: true,
	})
	assertConflict(err)
	assertAccounts(account(1, b.ID), account(2, b.ID), account(10, a.ID))
	bind(off.ID, 1)
	assertAccounts(account(2, b.ID), account(10, a.ID))
	bind(a.ID, 9)
	assertAccounts(account(2, b.ID), account(9, a.ID), account(10, a.ID))
	bind(0, 2)
	assertAccounts(account(9, a.ID), account(10, a.ID))
	_, err = db.ExecContext(ctx, `UPDATE accounts SET deleted_at=CURRENT_TIMESTAMP WHERE id=10`)
	require.NoError(t, err)
	assertAccounts(account(9, a.ID))
	_, err = db.ExecContext(ctx, `UPDATE accounts SET platform='gemini' WHERE id=9`)
	require.NoError(t, err)
	assertAccounts()
	bound := get(a.ID)
	assertConflict(repo.DeleteUpstreamChannel(ctx, a.ID, service.UpstreamChannelMutation{Revision: &bound.Revision, Confirm: true}))
	oldRevision := defaultChannel.Revision - 1
	assertConflict(repo.DeleteUpstreamChannel(ctx, defaultChannel.ID, service.UpstreamChannelMutation{Revision: &oldRevision, Confirm: true}))
	require.ErrorIs(t, repo.DeleteUpstreamChannel(ctx, defaultChannel.ID, service.UpstreamChannelMutation{Revision: &defaultChannel.Revision}), service.ErrUpstreamChannelValidation)
	require.NoError(t, repo.DeleteUpstreamChannel(ctx, defaultChannel.ID, service.UpstreamChannelMutation{Revision: &defaultChannel.Revision, Confirm: true}))
	_, err = repo.GetUpstreamChannel(ctx, defaultChannel.ID)
	require.ErrorIs(t, err, service.ErrUpstreamChannelNotFound)
	require.Equal(t, usageBefore, usageSnapshot(), "switches, ownership changes, and channel deletion must not rewrite usage or costs")
}
