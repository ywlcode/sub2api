package service

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpsCleanupDisabledDoesNotQueryDatabase(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := &OpsCleanupService{db: db, cfg: &config.Config{}}
	counts, err := svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, opsCleanupDeletedCounts{}, counts)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpsCleanupPlan(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		days         int
		wantOK       bool
		wantTruncate bool
		wantCutoff   time.Time
	}{
		{name: "negative skips", days: -1, wantOK: false},
		{name: "zero truncates", days: 0, wantOK: true, wantTruncate: true},
		{name: "positive yields past cutoff", days: 7, wantOK: true, wantCutoff: now.AddDate(0, 0, -7)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cutoff, truncate, ok := opsCleanupPlan(now, tc.days)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if truncate != tc.wantTruncate {
				t.Fatalf("truncate = %v, want %v", truncate, tc.wantTruncate)
			}
			if !tc.wantTruncate && !cutoff.Equal(tc.wantCutoff) {
				t.Fatalf("cutoff = %v, want %v", cutoff, tc.wantCutoff)
			}
		})
	}
}

func TestIsMissingRelationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not missing", err: nil, want: false},
		{name: "match relation does not exist", err: fakeErr(`pq: relation "ops_error_logs" does not exist`), want: true},
		{name: "match case-insensitive", err: fakeErr(`ERROR: Relation "x" Does Not Exist`), want: true},
		{name: "non-matching error", err: fakeErr("connection refused"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingRelationError(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func TestDeleteOldRowsByCTIDProtectsUnfinishedBillingEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	pattern := `(?s)WITH batch AS \(.*state IN \('complete','legacy'\).*DELETE FROM usage_billing_events`
	mock.ExpectExec(pattern).WithArgs(sqlmock.AnyArg(), 5000).WillReturnResult(sqlmock.NewResult(0, 1))

	deleted, err := deleteOldRowsByCTID(context.Background(), db, "usage_billing_events", "completed_at", time.Now(), "state IN ('complete','legacy')", 5000)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestArchiveOldUsageBillingDedupBatchesUntilDrained(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	pattern := `(?s)WITH victims AS \(.*INSERT INTO usage_billing_dedup_archive.*DELETE FROM usage_billing_dedup`
	mock.ExpectExec(pattern).WithArgs(sqlmock.AnyArg(), 5000).WillReturnResult(sqlmock.NewResult(0, 5000))
	mock.ExpectExec(pattern).WithArgs(sqlmock.AnyArg(), 5000).WillReturnResult(sqlmock.NewResult(0, 12))

	archived, err := archiveOldUsageBillingDedup(context.Background(), db, time.Now(), 5000)
	require.NoError(t, err)
	require.EqualValues(t, 5012, archived)
	require.NoError(t, mock.ExpectationsWereMet())
}
