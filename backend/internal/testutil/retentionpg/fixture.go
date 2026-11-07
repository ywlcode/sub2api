//go:build retentionintegration

// Package retentionpg is used only by the opt-in local retention database tests.
package retentionpg

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Open refuses non-loopback hosts and any database other than the disposable
// database created by tools/retention-test/run.sh. Never use a production DSN.
func Open(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("RETENTION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("run tools/retention-test/run.sh with an isolated PostgreSQL")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "localhost"}, u.Hostname())
	require.Equal(t, "/sub2api_retention_test", u.Path)
	require.Empty(t, u.Query().Get("host"))
	require.Empty(t, u.Query().Get("hostaddr"))
	require.Empty(t, u.Query().Get("service"))
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	return db
}

// Private clones migrated table definitions and indexes into a private schema.
// search_path deliberately excludes public so a missing fixture cannot cause
// cleanup to delete rows from another test. Triggers/FKs are not copied by LIKE;
// the whole-worker backup/replay scenario separately uses the migrated tables.
func Private(t *testing.T, tables ...string) *sql.DB {
	t.Helper()
	admin := Open(t)
	schema := fmt.Sprintf("retention_%d", time.Now().UnixNano())
	Exec(t, admin, "CREATE SCHEMA "+pq.QuoteIdentifier(schema))
	u, err := url.Parse(os.Getenv("RETENTION_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		Exec(t, admin, "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
	})
	for _, table := range tables {
		Exec(t, db, "CREATE TABLE "+pq.QuoteIdentifier(table)+" (LIKE public."+pq.QuoteIdentifier(table)+" INCLUDING ALL)")
	}
	return db
}

func Exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), query, args...)
	require.NoError(t, err)
}

func Count(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}
