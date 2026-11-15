package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type cleanupSettingsFailure struct {
	SettingRepository
	read func(context.Context) (map[string]string, error)
}

func (s cleanupSettingsFailure) GetMultiple(ctx context.Context, _ []string) (map[string]string, error) {
	return s.read(ctx)
}

type cleanupHeartbeatCapture struct {
	OpsRepository
	last *OpsUpsertJobHeartbeatInput
}

func (r *cleanupHeartbeatCapture) UpsertJobHeartbeat(_ context.Context, in *OpsUpsertJobHeartbeatInput) error {
	r.last = in
	return nil
}

func TestOpsCleanupUnreadableSettingsNeverDeletes(t *testing.T) {
	for _, name := range []string{"read failure", "invalid advanced JSON", "invalid runtime JSON"} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			settings := cleanupSettingsFailure{read: func(context.Context) (map[string]string, error) {
				switch name {
				case "read failure":
					return nil, errors.New("settings unavailable")
				case "invalid advanced JSON":
					return map[string]string{SettingKeyOpsAdvancedSettings: "{"}, nil
				default:
					return map[string]string{SettingKeyOpsRuntimeLogConfig: "{"}, nil
				}
			}}
			heartbeat := &cleanupHeartbeatCapture{}
			svc := NewOpsCleanupService(heartbeat, db, &config.Config{Ops: config.OpsConfig{
				Cleanup: config.OpsCleanupConfig{Enabled: true},
			}}, nil, settings)
			svc.runScheduled()
			require.NotNil(t, heartbeat.last)
			require.Contains(t, *heartbeat.last.LastError, "read cleanup settings")
			require.Nil(t, heartbeat.last.LastSuccessAt)
			require.NoError(t, mock.ExpectationsWereMet()) // No lock or deletion query.
		})
	}
}

func TestOpsCleanupStopCancelsRunAndOverlapIsSkipped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	entered, done := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	settings := cleanupSettingsFailure{read: func(ctx context.Context) (map[string]string, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := NewOpsCleanupService(&cleanupHeartbeatCapture{}, db, &config.Config{}, nil, settings)
	go func() { svc.runScheduled(); close(done) }()
	<-entered
	svc.runScheduled()
	// Stop must cancel even when the settings read is blocked.
	go svc.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup shutdown did not cancel the active run")
	}
	require.EqualValues(t, 1, calls.Load())
	require.NoError(t, mock.ExpectationsWereMet())
}

type cleanupAuditRepo struct {
	AuditLogRepository
	cutoffs []time.Time
}

func (r *cleanupAuditRepo) DeleteBefore(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	r.cutoffs = append(r.cutoffs, cutoff)
	if len(r.cutoffs) == 1 {
		return 5000, nil
	}
	return 3, nil
}

func TestAuditRetentionUsesPolicyAndSkipsOnReadFailure(t *testing.T) {
	for _, raw := range []string{"0", "60", "failure", "invalid"} {
		t.Run(raw, func(t *testing.T) {
			settings := newRuntimeSettingRepoStub()
			settings.getValueFn = func(string) (string, error) {
				if raw == "failure" {
					return "", errors.New("offline")
				}
				return raw, nil
			}
			repo := &cleanupAuditRepo{}
			svc := &AuditLogService{repo: repo, settingService: &SettingService{settingRepo: settings}}
			now := time.Now().UTC()
			err := svc.RunRetention(context.Background(), now)
			if raw == "failure" || raw == "invalid" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if raw == "60" {
				require.Equal(t, []time.Time{now.AddDate(0, 0, -60), now.AddDate(0, 0, -60)}, repo.cutoffs)
			} else {
				require.Empty(t, repo.cutoffs)
			}
		})
	}
}

type cleanupModerationRepo struct {
	ContentModerationRepository
	calls int
}

func (r *cleanupModerationRepo) CleanupExpiredLogs(_ context.Context, _, _ time.Time) (*ContentModerationCleanupResult, error) {
	r.calls++
	return &ContentModerationCleanupResult{FinishedAt: time.Now(), DeletedHit: 4}, nil
}

func TestOpsCleanupIsSingleOwnerOfHistoryRetention(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	settings := newRuntimeSettingRepoStub()
	settings.values[SettingKeyAuditLogRetentionDays] = "60"
	dashboardRepo := &dashboardAggregationRepoTestStub{}
	auditRepo := &cleanupAuditRepo{}
	moderationRepo := &cleanupModerationRepo{}
	svc := NewOpsCleanupService(nil, db, &config.Config{}, nil, settings)
	svc.dashboardSvc = &DashboardAggregationService{repo: dashboardRepo, settingRepo: settings,
		cfg: config.DashboardAggregationConfig{Enabled: true, Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 90}}}
	svc.auditSvc = &AuditLogService{repo: auditRepo, settingService: &SettingService{settingRepo: settings}}
	svc.moderationSvc = &ContentModerationService{repo: moderationRepo, settingRepo: settings}
	_, err = svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, dashboardRepo.cleanupUsageCalls)
	require.Empty(t, auditRepo.cutoffs)
	require.Zero(t, moderationRepo.calls)

	svc.effective.Enabled = true
	for i := 0; i < 9; i++ {
		mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	}
	for i := 0; i < 5; i++ {
		mock.ExpectExec("WITH (batch|victims)").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	_, err = svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, dashboardRepo.cleanupUsageCalls)
	require.Equal(t, 1, dashboardRepo.cleanupAggregateCalls)
	require.Len(t, auditRepo.cutoffs, 2)
	require.Equal(t, 1, moderationRepo.calls)
	require.EqualValues(t, 4, svc.moderationSvc.lastCleanupDeletedHit.Load())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpsCleanupKeepsDedupWhenUsageCleanupFails(t *testing.T) {
	for _, usageFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful history cleanup", true: "failed history cleanup"}[usageFails], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			cfg := &config.Config{DashboardAgg: config.DashboardAggregationConfig{Enabled: true,
				Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 90}}}
			svc := NewOpsCleanupService(nil, db, cfg, nil, nil)
			svc.effective.Enabled = true
			repo := &dashboardAggregationRepoTestStub{}
			failure := errors.New("usage deletion failed")
			if usageFails {
				repo.cleanupUsageErr = failure
			}
			svc.dashboardSvc = &DashboardAggregationService{repo: repo, cfg: cfg.DashboardAgg}
			for i := 0; i < 9; i++ {
				mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			}
			for i := 0; i < 5; i++ {
				mock.ExpectExec("WITH (batch|victims)").WillReturnResult(sqlmock.NewResult(0, 0))
			}
			if !usageFails {
				mock.ExpectExec("(?s)WITH batch AS.*DELETE FROM usage_billing_dedup_archive").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			counts, err := svc.runCleanupOnce(context.Background())
			if usageFails {
				require.ErrorIs(t, err, failure)
				require.NotContains(t, err.Error(), "was not expected")
				require.Zero(t, counts.billingDedupDeleted)
			} else {
				require.NoError(t, err)
				require.EqualValues(t, 1, counts.billingDedupDeleted)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCleanupAdvisoryLockDiscardsSessionOnUnlockFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs(int64(42)).WillReturnRows(sqlmock.NewRows([]string{"acquired"}).AddRow(true))
	release, acquired := tryAcquireDBAdvisoryLock(context.Background(), db, 42)
	require.True(t, acquired)
	mock.ExpectExec("SELECT pg_advisory_unlock").WithArgs(int64(42)).WillReturnError(errors.New("connection interrupted"))
	mock.ExpectClose()
	release()
	release() // Repeated release must not unlock another run or reuse the session.
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardRetentionDoesNotOverlapAggregation(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	svc := &DashboardAggregationService{repo: repo, running: 1}
	require.ErrorIs(t, svc.RunRetention(context.Background(), time.Now()), errRetentionBusy)
	require.Zero(t, repo.cleanupUsageCalls)
	require.Zero(t, repo.cleanupAggregateCalls)
}

func TestOpsCleanupScheduleIsIndependentOfMetricsCollection(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	cfg := &config.Config{Ops: config.OpsConfig{Enabled: false, Cleanup: config.OpsCleanupConfig{
		Enabled: true, Schedule: "0 0 1 1 *",
	}}}
	svc := NewOpsCleanupService(&cleanupHeartbeatCapture{}, db, cfg, nil, nil)
	svc.Start()
	defer svc.Stop()
	require.NotNil(t, svc.cron, "history cleanup uses its own enable switch")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpsCleanupRetriesBusyHistoryWithinRunBudget(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregation finishes", true: "shutdown while waiting"}[canceled], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			for i := 0; i < 9; i++ {
				mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			}
			for i := 0; i < 5; i++ {
				mock.ExpectExec("WITH (batch|victims)").WillReturnResult(sqlmock.NewResult(0, 0))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc := NewOpsCleanupService(nil, db, &config.Config{}, nil, nil)
			svc.effective.Enabled = true
			calls := 0
			svc.maintenance = []opsCleanupMaintenance{{"busy history", func(context.Context, time.Time) error {
				calls++
				if canceled {
					time.AfterFunc(10*time.Millisecond, cancel)
				}
				if calls == 1 {
					return errRetentionBusy
				}
				return nil
			}}}
			_, err = svc.runCleanupOnce(ctx)
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				require.Equal(t, 2, calls)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2RetentionUsesAggregationLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT pg_try_advisory_lock").
		WithArgs(hashAdvisoryLockID(channelMonitorV2AggregatorLockKey)).
		WillReturnRows(sqlmock.NewRows([]string{"acquired"}).AddRow(false))
	svc := NewChannelMonitorV2Aggregator(&channelMonitorV2RepoStub{}, db, nil)
	require.ErrorIs(t, svc.RunRetention(context.Background(), time.Now()), errRetentionBusy)
	require.NoError(t, mock.ExpectationsWereMet())
}
