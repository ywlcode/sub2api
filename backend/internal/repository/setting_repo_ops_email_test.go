//go:build unit

package repository

import (
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestConditionalOpsEmailConfigWrite(t *testing.T) {
	previous := "loaded JSON"
	for _, tc := range []struct {
		name     string
		previous *string
		matched  int64
	}{
		{"first save", nil, 1}, {"concurrent first save", nil, 0},
		{"current settings", &previous, 1}, {"settings changed after validation", &previous, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := settingRepository{client: ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
			const next = "next JSON"
			var write *sqlmock.ExpectedExec
			if tc.previous == nil {
				write = mock.ExpectExec("INSERT INTO settings.*ON CONFLICT .* DO NOTHING").WithArgs(service.SettingKeyOpsEmailNotificationConfig, next)
			} else {
				write = mock.ExpectExec(`UPDATE settings .* WHERE key = \$1 AND value = \$3`).WithArgs(service.SettingKeyOpsEmailNotificationConfig, next, *tc.previous)
			}
			write.WillReturnResult(sqlmock.NewResult(0, tc.matched))
			err = r.CompareAndSetOpsEmailConfig(t.Context(), tc.previous, next)
			if tc.matched == 0 {
				require.ErrorIs(t, err, service.ErrReportRevisionConflict)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
