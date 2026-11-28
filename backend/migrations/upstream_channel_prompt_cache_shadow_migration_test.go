package migrations

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const upstreamChannelPromptCacheShadowMigration = "396_upstream_channel_prompt_cache_shadow.sql"

func TestUpstreamChannelPromptCacheShadowMigration(t *testing.T) {
	content, err := FS.ReadFile(upstreamChannelPromptCacheShadowMigration)
	require.NoError(t, err)
	statement := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, statement, "prompt_cache_shadow_enabled BOOLEAN NOT NULL DEFAULT false")
	require.Contains(t, statement, "COMMENT ON TABLE upstream_channels")
	require.Contains(t, statement, "COMMENT ON COLUMN upstream_channels.prompt_cache_shadow_enabled")
	require.Contains(t, statement, "observation only")
	require.Contains(t, statement, "billing or routing")
	require.NotContains(t, strings.ToLower(statement), "usage_logs")
}

// This uses only session-local temporary tables on an explicitly supplied test
// server. It never starts Docker or modifies an application schema.
func TestUpstreamChannelPromptCacheShadowMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("UPSTREAM_CHANNEL_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("SUB2API_TEST_POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("set UPSTREAM_CHANNEL_TEST_POSTGRES_DSN to a disposable PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE accounts(id BIGINT PRIMARY KEY);
SET search_path TO pg_temp;
CREATE TEMP TABLE usage_logs(id BIGINT PRIMARY KEY, actual_cost NUMERIC, input_tokens BIGINT);
INSERT INTO usage_logs VALUES(1, 0.123456, 1234)`)
	require.NoError(t, err)
	base, err := FS.ReadFile("312_upstream_channel_entities.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, strings.ReplaceAll(string(base), "CREATE TABLE IF NOT EXISTS", "CREATE TEMP TABLE IF NOT EXISTS"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO upstream_channels(code,name,revision) VALUES('legacy-a','Legacy A',4),('legacy-b','Legacy B',9)`)
	require.NoError(t, err)
	var usageBefore string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM usage_logs u`).Scan(&usageBefore))
	migration, err := FS.ReadFile(upstreamChannelPromptCacheShadowMigration)
	require.NoError(t, err)

	// The runner's transaction must roll back both the column and the backfill.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	var enabled bool
	var revision int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT prompt_cache_shadow_enabled,revision FROM upstream_channels WHERE code='legacy-a'`).Scan(&enabled, &revision))
	require.True(t, enabled)
	require.EqualValues(t, 5, revision, "backfill invalidates pre-migration confirmations")
	require.NoError(t, tx.Rollback())
	var columns int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_attribute WHERE attrelid='upstream_channels'::regclass AND attname='prompt_cache_shadow_enabled' AND NOT attisdropped`).Scan(&columns))
	require.Zero(t, columns)

	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	var enabledCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM upstream_channels WHERE prompt_cache_shadow_enabled`).Scan(&enabledCount))
	require.Equal(t, 2, enabledCount, "existing channels retain observation")
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO upstream_channels(code,name) VALUES('new','New') RETURNING prompt_cache_shadow_enabled,revision`).Scan(&enabled, &revision))
	require.False(t, enabled)
	require.EqualValues(t, 1, revision)
	_, err = db.ExecContext(ctx, `UPDATE upstream_channels SET prompt_cache_shadow_enabled=false,revision=revision+1 WHERE code='legacy-a'`)
	require.NoError(t, err)

	// Replay must preserve explicit false, the new-row default, and CAS revisions.
	for range 2 {
		_, err = db.ExecContext(ctx, string(migration))
		require.NoError(t, err)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT prompt_cache_shadow_enabled,revision FROM upstream_channels WHERE code='legacy-a'`).Scan(&enabled, &revision))
		require.False(t, enabled)
		require.EqualValues(t, 6, revision)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT prompt_cache_shadow_enabled,revision FROM upstream_channels WHERE code='legacy-b'`).Scan(&enabled, &revision))
		require.True(t, enabled)
		require.EqualValues(t, 10, revision)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT prompt_cache_shadow_enabled,revision FROM upstream_channels WHERE code='new'`).Scan(&enabled, &revision))
		require.False(t, enabled)
		require.EqualValues(t, 1, revision)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO upstream_channels(code,name,prompt_cache_shadow_enabled) VALUES('invalid','Invalid',NULL)`)
	require.ErrorContains(t, err, "null value")
	var tableComment, columnComment string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT obj_description('upstream_channels'::regclass), col_description('upstream_channels'::regclass, attnum) FROM pg_attribute WHERE attrelid='upstream_channels'::regclass AND attname='prompt_cache_shadow_enabled' AND NOT attisdropped`).Scan(&tableComment, &columnComment))
	for _, comment := range []string{tableComment, columnComment} {
		require.Contains(t, comment, "observation only")
		require.Contains(t, comment, "billing or routing")
	}
	var usageAfter string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM usage_logs u`).Scan(&usageAfter))
	require.Equal(t, usageBefore, usageAfter)
}
