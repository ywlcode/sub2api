package repository

import (
	"context"
	"database/sql"
	"io"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectCompactSnapshot(mock sqlmock.Sqlmock) {
	expectBackupMigrationLock(mock)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_export_snapshot()")).
		WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow("00000003-00000001-1"))
	mock.ExpectQuery("WITH RECURSIVE excluded").WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"name", "referenced"}).AddRow("public.ops_error_logs", false).AddRow("public.usage_billing_events", false))
	mock.ExpectQuery("SELECT string_agg").WillReturnRows(sqlmock.NewRows([]string{"columns"}).AddRow("request_id, state"))
}

func TestCompactBackupCommandFailureAndEarlyClose(t *testing.T) {
	for _, test := range []string{"dump_failure", "copy_failure", "early_close"} {
		t.Run(test, func(t *testing.T) {
			dumper, mock := newTestPgDumper(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if test == "early_close" {
					return exec.CommandContext(ctx, "sleep", "30")
				}
				if (test == "dump_failure" && name == "pg_dump") || (test == "copy_failure" && name == "psql") {
					return exec.CommandContext(ctx, "sh", "-c", "exit 7")
				}
				return exec.CommandContext(ctx, "sh", "-c", "printf 'schema;\\n'")
			})
			expectCompactSnapshot(mock)
			mock.ExpectRollback()
			expectBackupMigrationUnlock(mock)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := dumper.DumpCompact(ctx)
			require.NoError(t, err)
			if test == "early_close" {
				_, err = io.ReadFull(r, make([]byte, 10))
				require.NoError(t, err)
			} else {
				_, err = io.ReadAll(r)
				require.ErrorContains(t, err, "exit status 7")
			}
			require.Error(t, r.Close())
			require.Error(t, r.Close())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCompactBackupSnapshotFailureReleasesMigrationLock(t *testing.T) {
	dumper, mock := newTestPgDumper(t, nil)
	expectBackupMigrationLock(mock)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT pg_export_snapshot").WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
	expectBackupMigrationUnlock(mock)
	r, err := dumper.DumpCompact(context.Background())
	require.Error(t, err)
	require.Nil(t, r)
	require.NoError(t, mock.ExpectationsWereMet())
}
