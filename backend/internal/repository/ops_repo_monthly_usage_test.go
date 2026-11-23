package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// The deployed exporter uses psql/COPY. Allow only the transport differences
// between those queries and the SELECTs embedded in the backend binary.
func TestMonthlyUsageQueriesMatchDeployedExporter(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("../../../deploy/monthly-usage/" + name)
		require.NoError(t, err)
		return strings.NewReplacer(
			":'month'", "$1",
			"0::bigint AS department_attribute_id", "$2::bigint AS department_attribute_id",
		).Replace(string(b))
	}
	require.Equal(t, read("headcounts.sql"), monthlyHeadcountsSQL)
	src := read("usage.sql")
	header, body, ok := strings.Cut(src, "COPY (\n")
	require.True(t, ok)
	require.Contains(t, header, "\\set email_pattern '"+monthlyUsageEmailPattern+"'\n")
	body, ok = strings.CutSuffix(body, ") TO STDOUT WITH CSV HEADER;\n")
	require.True(t, ok)
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		lines = append(lines, strings.TrimPrefix(line, "    "))
	}
	require.Equal(t, strings.ReplaceAll(strings.Join(lines, "\n"), ":'email_pattern'", "$3"), monthlyUsageSQL)
}

func TestMonthlyUsageReadFailureRollsBackBeforeOutput(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mock.ExpectClose(); require.NoError(t, db.Close()) })
	r := &opsRepository{db: db}
	mock.ExpectBegin()
	mock.ExpectQuery("WITH penalty_config").WithArgs("2024-02-01", int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"counts"}).AddRow("{}"))
	failure := errors.New("read interrupted")
	mock.ExpectQuery("WITH usage_buckets").WithArgs("2024-02-01", int64(0), monthlyUsageEmailPattern).
		WillReturnRows(sqlmock.NewRows([]string{"usage_day"}).AddRow("day").RowError(0, failure)).
		RowsWillBeClosed()
	mock.ExpectRollback()
	var out bytes.Buffer
	require.ErrorIs(t, r.BuildMonthlyUsageReport(context.Background(), "2024-02", "boscserver", 0, &out), failure)
	require.Empty(t, out.Bytes())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMonthlyUsageRejectsMalformedMonth(t *testing.T) {
	for _, month := range []string{"", "0000-01", "24-02", "2024-2", "2024-13", "2024-02-01"} {
		_, err := monthlyReportDate(month)
		require.Error(t, err, month)
	}
}
