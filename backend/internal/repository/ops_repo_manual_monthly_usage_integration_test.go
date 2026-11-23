//go:build integration

package repository

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func manualMonthlyUsagePools(t *testing.T) (*opsRepository, *opsRepository) {
	t.Helper()
	schema := "manual_report_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec("DROP SCHEMA " + schema + " CASCADE")
		require.NoError(t, err)
	})
	a := openGroupUsageRollupSchemaDB(t, schema, "America/Los_Angeles")
	b := openGroupUsageRollupSchemaDB(t, schema, "UTC")
	migration, err := migrations.FS.ReadFile("388_monthly_usage_email_deliveries.sql")
	require.NoError(t, err)
	for range 2 {
		_, err := a.Exec(string(migration))
		require.NoError(t, err)
	}
	return &opsRepository{db: a}, &opsRepository{db: b}
}

func TestManualMonthlyUsageConcurrentBegin(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		name := "different_requests"
		if sameID {
			name = "same_request"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			a, b := manualMonthlyUsagePools(t)
			const month = "2024-02"
			id := uuid.NewString()
			type result struct {
				job     *service.OpsMonthlyUsageJob
				created bool
				err     error
			}
			const workers = 12
			start := make(chan struct{})
			var wg sync.WaitGroup
			var results [workers]result
			for i := range workers {
				requestID := id
				if !sameID {
					requestID = uuid.NewString()
				}
				repo := []*opsRepository{a, b}[i%2]
				wg.Go(func() {
					<-start
					results[i].job, results[i].created, results[i].err = repo.BeginManualMonthlyUsage(ctx, requestID, month, 5)
				})
			}
			close(start)
			wg.Wait()
			var winner *service.OpsMonthlyUsageJob
			winners := 0
			for _, res := range results {
				if res.created {
					require.NoError(t, res.err)
					winner = res.job
					winners++
				} else if sameID {
					require.NoError(t, res.err)
					require.Equal(t, id, res.job.ID)
				} else {
					require.ErrorIs(t, res.err, service.ErrMonthlyUsageBusy)
					require.Nil(t, res.job)
				}
			}
			require.Equal(t, 1, winners)
			require.Equal(t, "running", winner.Status)
			retry, created, err := b.BeginManualMonthlyUsage(ctx, winner.ID, month, 99)
			require.NoError(t, err)
			require.False(t, created)
			require.Equal(t, winner, retry)
			_, _, err = a.BeginManualMonthlyUsage(ctx, winner.ID, "2024-03", 5)
			require.ErrorIs(t, err, service.ErrMonthlyUsageConflict)

			// Database uniqueness also protects callers outside this repository.
			_, err = b.db.ExecContext(ctx, `INSERT INTO monthly_usage_manual_jobs
(request_id,report_month,total) VALUES ($1,'2024-03-01',1)`, uuid.NewString())
			require.ErrorContains(t, err, "monthly_usage_manual_jobs_one_running")
		})
	}
}

func TestManualMonthlyUsageProgressAndExpiry(t *testing.T) {
	for _, trigger := range []string{"get", "begin", "late_update"} {
		t.Run(trigger, func(t *testing.T) {
			ctx := context.Background()
			a, b := manualMonthlyUsagePools(t)
			const month = "2024-02"
			id := uuid.NewString()
			_, err := b.GetManualMonthlyUsage(ctx, id)
			require.ErrorIs(t, err, service.ErrMonthlyUsageJobNotFound)
			require.ErrorIs(t, b.UpdateManualMonthlyUsage(ctx, &service.OpsMonthlyUsageJob{ID: id, Month: month}),
				service.ErrMonthlyUsageJobNotFound)

			job, created, err := a.BeginManualMonthlyUsage(ctx, id, month, 8)
			require.NoError(t, err)
			require.True(t, created)
			age := func(minutes int) {
				t.Helper()
				_, err := a.db.ExecContext(ctx, `UPDATE monthly_usage_manual_jobs SET
touched_at=now() - make_interval(mins => $2) WHERE request_id=$1`, id, minutes)
				require.NoError(t, err)
			}
			age(29)
			job.Accepted, job.Failed, job.Uncertain = 2, 1, 1
			require.NoError(t, a.UpdateManualMonthlyUsage(ctx, job))
			var refreshed bool
			require.NoError(t, b.db.QueryRowContext(ctx, `SELECT touched_at > now() - interval '1 minute'
FROM monthly_usage_manual_jobs WHERE request_id=$1`, id).Scan(&refreshed))
			require.True(t, refreshed)
			seen, err := b.GetManualMonthlyUsage(ctx, id)
			require.NoError(t, err)
			require.Equal(t, job, seen)
			age(31)
			next := uuid.NewString()
			switch trigger {
			case "begin":
				_, created, err = b.BeginManualMonthlyUsage(ctx, next, month, 1)
				require.NoError(t, err)
				require.True(t, created)
			case "late_update":
				require.Error(t, a.UpdateManualMonthlyUsage(ctx, job))
			}
			seen, err = b.GetManualMonthlyUsage(ctx, id)
			require.NoError(t, err)
			want := *job
			want.Status, want.Uncertain = "uncertain", 5
			require.Equal(t, &want, seen)
			require.Error(t, a.UpdateManualMonthlyUsage(ctx, job))
			retry, created, err := a.BeginManualMonthlyUsage(ctx, id, month, 8)
			require.NoError(t, err)
			require.False(t, created)
			require.Equal(t, &want, retry)
			if trigger != "begin" {
				_, created, err = b.BeginManualMonthlyUsage(ctx, next, month, 1)
				require.NoError(t, err)
				require.True(t, created)
			}
			newJob, err := b.GetManualMonthlyUsage(ctx, next)
			require.NoError(t, err)
			require.Equal(t, "running", newJob.Status)
			newJob.Status, newJob.Failed = "failed", 1
			require.NoError(t, b.UpdateManualMonthlyUsage(ctx, newJob))
			require.NoError(t, b.UpdateManualMonthlyUsage(ctx, newJob))
			newJob.Status = "running"
			require.Error(t, a.UpdateManualMonthlyUsage(ctx, newJob))

			ok, err := a.ClaimMonthlyUsageEmail(ctx, month, "test@example.invalid")
			require.NoError(t, err)
			require.True(t, ok)
			_, created, err = a.BeginManualMonthlyUsage(ctx, uuid.NewString(), month, 1)
			require.NoError(t, err)
			require.True(t, created)
		})
	}
}
