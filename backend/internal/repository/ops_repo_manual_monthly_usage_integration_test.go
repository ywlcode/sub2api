//go:build integration

package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
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
	_, err = a.Exec("CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL)")
	require.NoError(t, err)
	for _, file := range []string{
		"388_monthly_usage_email_deliveries.sql",
		"389_monthly_usage_delivery_outcomes.sql",
		"390_monthly_usage_manual_job_owner.sql",
	} {
		migration, err := migrations.FS.ReadFile(file)
		require.NoError(t, err)
		for range 2 {
			_, err := a.Exec(string(migration))
			require.NoError(t, err)
		}
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
					results[i].job, results[i].created, results[i].err = repo.BeginManualMonthlyUsage(ctx, requestID, month, i+1)
				})
			}
			close(start)
			wg.Wait()
			var winner *service.OpsMonthlyUsageJob
			winners := 0
			for i, res := range results {
				if res.created {
					require.NoError(t, res.err)
					require.Equal(t, i+1, res.job.Total)
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

// With binary_parameters=yes lib/pq sends Parse, Bind, Execute and Sync in one
// write. Hold that write's return until another session observes the commit,
// then close the socket before lib/pq can read any part of the reply.
type manualInsertACKDialer struct {
	once        sync.Once
	afterInsert func()
	offline     atomic.Bool
}

func (d *manualInsertACKDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialTimeout(network, address, 0)
}

func (d *manualInsertACKDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	if d.offline.Load() {
		return nil, errors.New("writer disconnected from PostgreSQL")
	}
	conn, err := net.DialTimeout(network, address, timeout)
	if err != nil {
		return nil, err
	}
	return &manualInsertACKConn{Conn: conn, dialer: d}, nil
}

type manualInsertACKConn struct {
	net.Conn
	dialer *manualInsertACKDialer
}

func (c *manualInsertACKConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && bytes.Contains(p, []byte("INSERT INTO monthly_usage_manual_jobs")) {
		c.dialer.once.Do(func() {
			defer c.Conn.Close()
			c.dialer.afterInsert()
		})
	}
	return n, err
}

func TestManualMonthlyUsageLostInsertACK(t *testing.T) {
	for _, name := range []string{"recover_after_disconnect", "claim_deadline_after_commit", "writer_stays_offline"} {
		t.Run(name, func(t *testing.T) {
			_, observer := manualMonthlyUsagePools(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var schema string
			require.NoError(t, observer.db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema))
			dsn, err := url.Parse(integrationDSN)
			require.NoError(t, err)
			params := dsn.Query()
			params.Set("search_path", schema)
			params.Set("binary_parameters", "yes")
			params.Set("sslmode", "disable")
			dsn.RawQuery = params.Encode()
			connector, err := pq.NewConnector(dsn.String())
			require.NoError(t, err)

			const month = "2024-02"
			id := uuid.NewString()
			claimCtx := ctx
			if name == "claim_deadline_after_commit" {
				var stopClaim context.CancelFunc
				claimCtx, stopClaim = context.WithTimeout(ctx, time.Second)
				defer stopClaim()
			}
			dropped := false
			dialer := &manualInsertACKDialer{}
			dialer.afterInsert = func() {
				dropped = true
				t.Run("committed_before_disconnect", func(t *testing.T) {
					require.Eventually(t, func() bool {
						var total int
						err := observer.db.QueryRowContext(ctx, "SELECT total FROM monthly_usage_manual_jobs WHERE request_id = $1", id).Scan(&total)
						return err == nil && total == 7
					}, 5*time.Second, 5*time.Millisecond)
					job, created, err := observer.BeginManualMonthlyUsage(ctx, id, month, 99)
					require.NoError(t, err)
					require.False(t, created)
					require.Equal(t, 7, job.Total)
				})
				if name == "claim_deadline_after_commit" {
					<-claimCtx.Done()
				}
				if name == "writer_stays_offline" {
					dialer.offline.Store(true)
				}
			}
			connector.Dialer(dialer)
			writer := &opsRepository{db: sql.OpenDB(connector)}
			t.Cleanup(func() { _ = writer.db.Close() })
			job, created, err := writer.BeginManualMonthlyUsage(claimCtx, id, month, 7)
			require.True(t, dropped)
			if name == "writer_stays_offline" {
				require.Error(t, err)
				require.NotErrorIs(t, err, service.ErrMonthlyUsageJobNotFound)
				require.False(t, created)
				require.Nil(t, job)
				_, err = observer.db.ExecContext(ctx, `UPDATE monthly_usage_manual_jobs
SET touched_at = now() - interval '31 minutes' WHERE request_id = $1`, id)
				require.NoError(t, err)
				job, created, err = observer.BeginManualMonthlyUsage(ctx, id, month, 99)
				require.NoError(t, err)
				require.False(t, created)
				require.Equal(t, "uncertain", job.Status)
				require.Equal(t, 7, job.Uncertain)
				return
			}
			require.NoError(t, err)
			require.True(t, created)
			require.Equal(t, id, job.ID)
			require.Equal(t, 7, job.Total)
		})
	}
}

func TestManualMonthlyUsageWriteError(t *testing.T) {
	a, _ := manualMonthlyUsagePools(t)
	job, created, err := a.BeginManualMonthlyUsage(t.Context(), uuid.NewString(), "2024-02", -1)
	var writeErr *pq.Error
	require.ErrorAs(t, err, &writeErr)
	require.Equal(t, pq.ErrorCode("23514"), writeErr.Code)
	require.NotErrorIs(t, err, service.ErrMonthlyUsageJobNotFound)
	require.False(t, created)
	require.Nil(t, job)
}

func TestManualMonthlyUsageLegacyOwner(t *testing.T) {
	ctx := t.Context()
	a, b := manualMonthlyUsagePools(t)
	id := uuid.NewString()
	_, err := a.db.ExecContext(ctx, `INSERT INTO monthly_usage_manual_jobs
(request_id, report_month, total) VALUES ($1, '2024-02-01', 3)`, id)
	require.NoError(t, err)
	job, created, err := b.BeginManualMonthlyUsage(ctx, id, "2024-02", 5)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, "running", job.Status)
	require.Equal(t, 3, job.Total)
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
