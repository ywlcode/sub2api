package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestUserGroupRequestPolicyCombinedRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rows   *sqlmock.Rows
		rpm    *int
		denied []string
		err    error
	}{
		{name: "absent", rows: sqlmock.NewRows([]string{"rpm_override", "denied_models"})},
		{name: "null", rows: sqlmock.NewRows([]string{"rpm_override", "denied_models"}).AddRow(nil, nil)},
		{name: "zero override", rows: sqlmock.NewRows([]string{"rpm_override", "denied_models"}).AddRow(0, []byte(`["gpt-5"]`)), rpm: new(int), denied: []string{"gpt-5"}},
		{name: "failure", err: errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() {
				mock.ExpectClose()
				require.NoError(t, db.Close())
			})
			query := mock.ExpectQuery(regexp.QuoteMeta(`SELECT rpm_override, denied_models FROM user_group_rate_multipliers WHERE user_id = $1 AND group_id = $2`)).WithArgs(int64(7), int64(2))
			if tc.err != nil {
				query.WillReturnError(tc.err)
			} else {
				query.WillReturnRows(tc.rows)
			}
			repo := &userGroupRateRepository{sql: db}
			rpm, denied, err := repo.GetRequestPolicy(context.Background(), 7, 2)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.rpm, rpm)
				require.Equal(t, tc.denied, denied)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
