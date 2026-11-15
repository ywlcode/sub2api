package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

const (
	opsCleanupJobName = "ops_cleanup"

	opsCleanupLeaderLockKeyDefault = "ops:cleanup:leader"
)

var opsCleanupCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
var errRetentionBusy = errors.New("history aggregation is running")

// OpsCleanupService periodically deletes old ops data to prevent unbounded DB growth.
//
// - Scheduling: 5-field cron spec (minute hour dom month dow).
// - Multi-instance: one database advisory lock for all cleanup steps.
// - Safety: deletes in batches to avoid long transactions.
//
// 附带：在 runCleanupOnce 末尾调用 ChannelMonitorService.RunDailyMaintenance，
// 统一共享 cron schedule + leader lock + heartbeat，避免再引一套调度。
type OpsCleanupService struct {
	opsRepo           OpsRepository
	db                *sql.DB
	cfg               *config.Config
	channelMonitorSvc *ChannelMonitorService
	settingRepo       SettingRepository

	dashboardSvc  *DashboardAggregationService
	auditSvc      *AuditLogService
	moderationSvc *ContentModerationService
	maintenance   []opsCleanupMaintenance
	runMu         sync.Mutex
	cancelMu      sync.Mutex // cancellation must not wait for a settings DB read
	cancelRun     context.CancelFunc
	settingsErr   error

	// mu 守护 cron 实例切换 + effective 配置切换。
	// 这里不再用 startOnce/stopOnce，是因为 Reload 需要"停旧 cron 重启新 cron"，
	// 而 Once 一旦触发就无法再次执行；改为 started/stopped 布尔配合 mu。
	mu        sync.Mutex
	cron      *cron.Cron
	started   bool
	stopped   bool
	effective opsCleanupEffectiveConfig
}

// Extra histories reuse their existing policies but have no independent timer.
type opsCleanupMaintenance struct {
	name string
	run  func(context.Context, time.Time) error
}

type opsCleanupEffectiveConfig struct {
	config.OpsCleanupConfig
	SystemLogRetentionDays             int
	RequestConnectionRetentionDays     int
	RequestTimingRetentionDays         int
	CompletedBillingEventRetentionDays int
	BillingDedupHotRetentionDays       int
	BillingDedupArchiveRetentionDays   int
	UserTrafficRetentionDays           int
}

func NewOpsCleanupService(
	opsRepo OpsRepository,
	db *sql.DB,
	cfg *config.Config,
	channelMonitorSvc *ChannelMonitorService,
	settingRepo SettingRepository,
) *OpsCleanupService {
	return &OpsCleanupService{
		opsRepo:           opsRepo,
		db:                db,
		cfg:               cfg,
		channelMonitorSvc: channelMonitorSvc,
		settingRepo:       settingRepo,
	}
}

// Start 首次启动 cron 调度。Enabled / Schedule 由 effective 配置决定（settings 优先 cfg）。
// 重复调用幂等。
func (s *OpsCleanupService) Start() {
	if s == nil {
		return
	}
	// Cleanup now owns non-Ops histories too. Its own switch controls retention;
	// disabling metrics collection must not silently disable audit/usage cleanup.
	if s.opsRepo == nil || s.db == nil {
		logger.LegacyPrintf("service.ops_cleanup", "[OpsCleanup] not started (missing deps)")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	if err := s.applyScheduleLocked(context.Background()); err != nil {
		logger.LegacyPrintf("service.ops_cleanup", "[OpsCleanup] not started: %v", err)
	}
}

// Stop cancels active deletion before waiting, without holding the settings lock.
func (s *OpsCleanupService) Stop() {
	if s == nil {
		return
	}
	s.cancelMu.Lock()
	if s.cancelRun != nil {
		s.cancelRun()
	}
	s.mu.Lock()
	s.stopped = true
	c := s.cron
	s.cron = nil
	s.mu.Unlock()
	s.cancelMu.Unlock()
	if c != nil {
		select {
		case <-c.Stop().Done():
		case <-time.After(opsCleanupCronStopTimeout):
			logger.LegacyPrintf("service.ops_cleanup", "[OpsCleanup] cron stop timed out")
		}
	}
}

// Reload only replaces the schedule. The run mutex also covers an old cron job.
func (s *OpsCleanupService) stopCronLocked() {
	if s.cron != nil {
		s.cron.Stop()
		s.cron = nil
	}
}

// applyScheduleLocked 重新计算 effective 配置并按其 schedule 重建 cron。调用方持锁。
// 若 effective.Enabled=false（用户在 UI 关闭清理），停旧 cron 后直接返回，不创建新 cron。
func (s *OpsCleanupService) applyScheduleLocked(ctx context.Context) error {
	s.computeEffectiveLocked(ctx)

	if !s.effective.Enabled {
		s.stopCronLocked()
		logger.LegacyPrintf("service.ops_cleanup", "[OpsCleanup] cron disabled by settings")
		return nil
	}

	schedule := strings.TrimSpace(s.effective.Schedule)
	if schedule == "" {
		schedule = opsCleanupDefaultSchedule
	}

	loc := time.Local
	if s.cfg != nil && strings.TrimSpace(s.cfg.Timezone) != "" {
		if parsed, err := time.LoadLocation(strings.TrimSpace(s.cfg.Timezone)); err == nil && parsed != nil {
			loc = parsed
		}
	}

	c := cron.New(cron.WithParser(opsCleanupCronParser), cron.WithLocation(loc))
	if _, err := c.AddFunc(schedule, func() { s.runScheduled() }); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	s.stopCronLocked()
	c.Start()
	s.cron = c
	logger.LegacyPrintf("service.ops_cleanup",
		"[OpsCleanup] scheduled (schedule=%q tz=%s retention_days=err:%d/system:%d/min:%d/hour:%d/connection:%d/timing:%d/billing_event:%d/dedup_hot:%d/dedup_archive:%d/traffic:%d)",
		schedule, loc.String(),
		s.effective.ErrorLogRetentionDays,
		s.effective.SystemLogRetentionDays,
		s.effective.MinuteMetricsRetentionDays,
		s.effective.HourlyMetricsRetentionDays,
		s.effective.RequestConnectionRetentionDays,
		s.effective.RequestTimingRetentionDays,
		s.effective.CompletedBillingEventRetentionDays,
		s.effective.BillingDedupHotRetentionDays,
		s.effective.BillingDedupArchiveRetentionDays,
		s.effective.UserTrafficRetentionDays,
	)
	return nil
}

// Reload 重新读取 ops_advanced_settings.data_retention 并按新配置重建 cron。
// 适用于 admin 在 UI 修改清理设置后立即生效（schedule / enabled 改动需要 Reload；
// retention 改动 runScheduled 顶部也会刷新，下一次触发即生效）。
// 若 service 还未 Start 或已 Stop，Reload 不做任何事。
func (s *OpsCleanupService) Reload(ctx context.Context) error {
	if s == nil {
		return nil
	}
	// A longer retention window or disabling cleanup must also stop a run
	// already deleting with the old policy. Committed batches remain committed.
	s.cancelMu.Lock()
	if s.cancelRun != nil {
		s.cancelRun()
	}
	s.cancelMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopped {
		return nil
	}
	return s.applyScheduleLocked(ctx)
}

// computeEffectiveLocked 计算"生效配置"并写入 s.effective。调用方持锁。
//
// 优先级：UI 写入的 settings.ops_advanced_settings.data_retention（权威）覆盖 cfg.Ops.Cleanup 的副本。
//   - Enabled：settings 直接覆盖
//   - Schedule：settings 非空时覆盖，否则保留 cfg
//   - *RetentionDays：settings >=0 时覆盖（包括 0=TRUNCATE），<0 沿用 cfg
//
// System log retention has a separate owner: settings.ops_runtime_log_config.retention_days.
// Missing fields use compatible defaults. Read/parse failures are kept separately
// so scheduling can recover, but runScheduled never deletes using a fallback.
func (s *OpsCleanupService) computeEffectiveLocked(ctx context.Context) {
	s.settingsErr = nil
	defaults := defaultOpsAdvancedSettings().DataRetention
	base := opsCleanupEffectiveConfig{
		SystemLogRetentionDays:             defaultOpsRuntimeLogConfig(s.cfg).RetentionDays,
		RequestConnectionRetentionDays:     defaults.RequestConnectionRetentionDays,
		RequestTimingRetentionDays:         defaults.RequestTimingRetentionDays,
		CompletedBillingEventRetentionDays: defaults.CompletedBillingEventRetentionDays,
		BillingDedupHotRetentionDays:       defaults.BillingDedupHotRetentionDays,
		BillingDedupArchiveRetentionDays:   defaults.BillingDedupArchiveRetentionDays,
		UserTrafficRetentionDays:           defaults.UserTrafficRetentionDays,
	}
	if s.cfg != nil {
		base.OpsCleanupConfig = s.cfg.Ops.Cleanup
	}
	defer func() { s.effective = base }()

	if s.settingRepo == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyOpsAdvancedSettings,
		SettingKeyOpsRuntimeLogConfig,
	})
	if err != nil {
		logger.LegacyPrintf("service.ops_cleanup",
			"[OpsCleanup] read cleanup settings failed; deletion will be skipped: %v", err)
		s.settingsErr = err
		return
	}
	if raw, ok := values[SettingKeyOpsAdvancedSettings]; ok {
		adv := defaultOpsAdvancedSettingsForConfig(s.cfg)
		if err := json.Unmarshal([]byte(raw), adv); err != nil {
			logger.LegacyPrintf("service.ops_cleanup",
				"[OpsCleanup] parse advanced settings failed; deletion will be skipped: %v", err)
			s.settingsErr = errors.Join(s.settingsErr, err)
		} else {
			dr := adv.DataRetention
			// The extended datasets do not support zero-day truncation. API writes
			// normalize zero/missing values, and this duplicate guard protects the
			// cleanup worker from hand-edited legacy settings as well.
			if dr.RequestConnectionRetentionDays <= 0 {
				dr.RequestConnectionRetentionDays = defaults.RequestConnectionRetentionDays
			}
			if dr.RequestTimingRetentionDays <= 0 {
				dr.RequestTimingRetentionDays = defaults.RequestTimingRetentionDays
			}
			if dr.CompletedBillingEventRetentionDays <= 0 {
				dr.CompletedBillingEventRetentionDays = defaults.CompletedBillingEventRetentionDays
			}
			if dr.BillingDedupHotRetentionDays <= 0 {
				dr.BillingDedupHotRetentionDays = defaults.BillingDedupHotRetentionDays
			}
			if dr.BillingDedupArchiveRetentionDays <= 0 {
				dr.BillingDedupArchiveRetentionDays = defaults.BillingDedupArchiveRetentionDays
			}
			if dr.UserTrafficRetentionDays <= 0 {
				dr.UserTrafficRetentionDays = defaults.UserTrafficRetentionDays
			}
			base.Enabled = dr.CleanupEnabled
			if sched := strings.TrimSpace(dr.CleanupSchedule); sched != "" {
				base.Schedule = sched
			}
			if dr.ErrorLogRetentionDays >= 0 {
				base.ErrorLogRetentionDays = dr.ErrorLogRetentionDays
			}
			if dr.MinuteMetricsRetentionDays >= 0 {
				base.MinuteMetricsRetentionDays = dr.MinuteMetricsRetentionDays
			}
			if dr.HourlyMetricsRetentionDays >= 0 {
				base.HourlyMetricsRetentionDays = dr.HourlyMetricsRetentionDays
			}
			base.RequestConnectionRetentionDays = dr.RequestConnectionRetentionDays
			base.RequestTimingRetentionDays = dr.RequestTimingRetentionDays
			base.CompletedBillingEventRetentionDays = dr.CompletedBillingEventRetentionDays
			base.BillingDedupHotRetentionDays = dr.BillingDedupHotRetentionDays
			base.BillingDedupArchiveRetentionDays = dr.BillingDedupArchiveRetentionDays
			base.UserTrafficRetentionDays = dr.UserTrafficRetentionDays
		}
	}
	if raw, ok := values[SettingKeyOpsRuntimeLogConfig]; ok {
		defaults := defaultOpsRuntimeLogConfig(s.cfg)
		runtimeCfg := *defaults
		if err := json.Unmarshal([]byte(raw), &runtimeCfg); err != nil {
			logger.LegacyPrintf("service.ops_cleanup",
				"[OpsCleanup] parse runtime log config failed; deletion will be skipped: %v", err)
			s.settingsErr = errors.Join(s.settingsErr, err)
		} else {
			normalizeOpsRuntimeLogConfig(&runtimeCfg, defaults)
			if runtimeCfg.RetentionDays >= 1 && runtimeCfg.RetentionDays <= 3650 {
				base.SystemLogRetentionDays = runtimeCfg.RetentionDays
			}
		}
	}
}

// snapshotEffective 取一份 effective 副本（runCleanupOnce 等读路径使用）。
func (s *OpsCleanupService) snapshotEffective() (opsCleanupEffectiveConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effective, s.settingsErr
}

// refreshEffectiveBeforeRun 在 cron 触发时刷新 effective，让 retention 改动当次即生效。
// schedule 改动不影响当次（cron 调度由库管理，需要 Reload 才换 schedule）。
func (s *OpsCleanupService) refreshEffectiveBeforeRun(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.computeEffectiveLocked(ctx)
	return s.settingsErr
}

func (s *OpsCleanupService) runScheduled() {
	if s == nil || s.db == nil || s.opsRepo == nil || !s.runMu.TryLock() {
		return
	}
	defer s.runMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), opsCleanupRunTimeout)
	s.cancelMu.Lock()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		s.cancelMu.Unlock()
		cancel()
		return
	}
	s.cancelRun = cancel
	s.mu.Unlock()
	s.cancelMu.Unlock()
	defer func() { cancel(); s.cancelMu.Lock(); s.cancelRun = nil; s.cancelMu.Unlock() }()

	// Recheck enabled as well as retention: another instance may have disabled
	// cleanup while this instance still has the old cron schedule installed.
	if err := s.refreshEffectiveBeforeRun(ctx); err != nil {
		s.recordHeartbeatError(time.Now().UTC(), 0, fmt.Errorf("read cleanup settings: %w", err))
		return
	}
	effective, err := s.snapshotEffective()
	if err != nil {
		s.recordHeartbeatError(time.Now().UTC(), 0, fmt.Errorf("read cleanup settings: %w", err))
		return
	}
	if !effective.Enabled {
		return
	}

	release, ok := s.tryAcquireLeaderLock(ctx)
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}

	startedAt := time.Now().UTC()
	runAt := startedAt

	counts, err := s.runCleanupOnce(ctx)
	if err != nil {
		s.recordHeartbeatError(runAt, time.Since(startedAt), err)
		logger.LegacyPrintf("service.ops_cleanup", "[OpsCleanup] cleanup failed: %v", err)
		return
	}
	s.recordHeartbeatSuccess(runAt, time.Since(startedAt), counts)
	logger.L().Info("[OpsCleanup] cleanup complete",
		zap.String("component", "service.ops_cleanup"),
		zap.String("deleted_counts", counts.String()),
	)
}

func (s *OpsCleanupService) runCleanupOnce(ctx context.Context) (opsCleanupDeletedCounts, error) {
	out := opsCleanupDeletedCounts{}
	if s == nil || s.db == nil || s.cfg == nil {
		return out, nil
	}

	effective, err := s.snapshotEffective()
	if err != nil {
		return out, fmt.Errorf("read cleanup settings: %w", err)
	}
	if !effective.Enabled {
		return out, nil
	}

	now := time.Now().UTC()

	targets := []opsCleanupTarget{
		{effective.ErrorLogRetentionDays, "ops_error_logs", "created_at", false, &out.errorLogs},
		{effective.ErrorLogRetentionDays, "ops_ingress_reject_aggregates", "bucket_start", false, &out.ingressRejects},
		{effective.ErrorLogRetentionDays, "ops_alert_events", "created_at", false, &out.alertEvents},
		{effective.SystemLogRetentionDays, "ops_system_logs", "created_at", false, &out.systemLogs},
		{effective.SystemLogRetentionDays, "ops_system_log_cleanup_audits", "created_at", false, &out.logAudits},
		{effective.MinuteMetricsRetentionDays, "ops_system_metrics", "created_at", false, &out.systemMetrics},
		{effective.MinuteMetricsRetentionDays, "ops_network_minutes", "sampled_at", false, &out.networkMetrics},
		{effective.HourlyMetricsRetentionDays, "ops_metrics_hourly", "bucket_start", false, &out.hourlyPreagg},
		{effective.HourlyMetricsRetentionDays, "ops_metrics_daily", "bucket_date", true, &out.dailyPreagg},
	}

	// Each step has a bounded budget. One locked/broken table must not starve
	// unrelated retention, and all failures remain visible in the job heartbeat.
	var failures []error
	run := func(name string, fn func(context.Context) error) bool {
		if ctx.Err() != nil {
			return false
		}
		stepCtx, cancel := context.WithTimeout(ctx, opsCleanupStepTimeout)
		err := fn(stepCtx)
		// A daily task must not lose its only attempt merely because a minute
		// aggregation started at the same time. Wait within this step's budget.
		for errors.Is(err, errRetentionBusy) && stepCtx.Err() == nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-stepCtx.Done():
				timer.Stop()
				err = stepCtx.Err()
			case <-timer.C:
				err = fn(stepCtx)
			}
		}
		if err == nil {
			err = stepCtx.Err()
		}
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			return false
		}
		return true
	}
	for _, target := range targets {
		cutoff, truncate, ok := opsCleanupPlan(now, target.retentionDays)
		if !ok {
			continue
		}
		run(target.table, func(ctx context.Context) error {
			n, err := opsCleanupRunOne(ctx, s.db, truncate, cutoff, target.table, target.timeCol, target.castDate, opsCleanupBatchSize)
			*target.counter = n
			return err
		})
	}
	for _, target := range []struct {
		table, column, predicate string
		days                     int
		count                    *int64
	}{
		{"request_connection_traces", "started_at", "", effective.RequestConnectionRetentionDays, &out.requestConnections},
		{"request_timing_details", "created_at", "", effective.RequestTimingRetentionDays, &out.requestTimings},
		{"usage_billing_events", "completed_at", "state IN ('complete','legacy')", effective.CompletedBillingEventRetentionDays, &out.billingEvents},
		{"user_traffic_events", "started_at", "", effective.UserTrafficRetentionDays, &out.userTraffic},
	} {
		run(target.table, func(ctx context.Context) error {
			n, err := deleteOldRowsByCTID(ctx, s.db, target.table, target.column, now.AddDate(0, 0, -target.days), target.predicate, opsCleanupBatchSize)
			*target.count = n
			return err
		})
	}
	run("billing dedup archive", func(ctx context.Context) error {
		var err error
		out.billingDedupArchived, err = archiveOldUsageBillingDedup(ctx, s.db, now.AddDate(0, 0, -effective.BillingDedupHotRetentionDays), opsCleanupBatchSize)
		return err
	})
	// Expiring idempotency claims must follow successful usage-history cleanup.
	// Otherwise a failed usage sweep could leave old requests without protection.
	usageCleaned := true
	if s.dashboardSvc != nil {
		usageCleaned = run("usage history", func(ctx context.Context) error { return s.dashboardSvc.RunRetention(ctx, now) })
	}
	if usageCleaned {
		run("billing dedup expiry", func(ctx context.Context) error {
			days, err := s.billingDedupRetentionDays(ctx, effective)
			if err != nil || days == 0 {
				return err
			}
			out.billingDedupDeleted, err = deleteOldRowsByCTID(ctx, s.db, "usage_billing_dedup_archive", "created_at", now.AddDate(0, 0, -days), "", opsCleanupBatchSize)
			return err
		})
	}
	if s.auditSvc != nil {
		run("admin audit", func(ctx context.Context) error { return s.auditSvc.RunRetention(ctx, now) })
	}
	if s.moderationSvc != nil {
		run("content moderation", func(ctx context.Context) error { return s.moderationSvc.RunRetention(ctx, now) })
	}
	if s.channelMonitorSvc != nil {
		run("channel monitor", s.channelMonitorSvc.RunDailyMaintenance)
	}
	for _, task := range s.maintenance {
		run(task.name, func(ctx context.Context) error { return task.run(ctx, now) })
	}
	return out, errors.Join(append(failures, ctx.Err())...)
}

func (s *OpsCleanupService) tryAcquireLeaderLock(ctx context.Context) (func(), bool) {
	// A single lock domain prevents a Redis failure from admitting a second
	// cleaner via the database while another instance still owns a Redis lease.
	if s == nil {
		return nil, false
	}
	return tryAcquireDBAdvisoryLock(ctx, s.db, hashAdvisoryLockID(opsCleanupLeaderLockKeyDefault))
}

func (s *OpsCleanupService) recordHeartbeatSuccess(runAt time.Time, duration time.Duration, counts opsCleanupDeletedCounts) {
	if s == nil || s.opsRepo == nil {
		return
	}
	now := time.Now().UTC()
	durMs := duration.Milliseconds()
	result := truncateString(counts.String(), 2048)
	ctx, cancel := context.WithTimeout(context.Background(), opsCleanupHeartbeatTimeout)
	defer cancel()
	_ = s.opsRepo.UpsertJobHeartbeat(ctx, &OpsUpsertJobHeartbeatInput{
		JobName:        opsCleanupJobName,
		LastRunAt:      &runAt,
		LastSuccessAt:  &now,
		LastDurationMs: &durMs,
		LastResult:     &result,
	})
}

func (s *OpsCleanupService) recordHeartbeatError(runAt time.Time, duration time.Duration, err error) {
	if s == nil || s.opsRepo == nil || err == nil {
		return
	}
	now := time.Now().UTC()
	durMs := duration.Milliseconds()
	msg := truncateString(err.Error(), 2048)
	ctx, cancel := context.WithTimeout(context.Background(), opsCleanupHeartbeatTimeout)
	defer cancel()
	_ = s.opsRepo.UpsertJobHeartbeat(ctx, &OpsUpsertJobHeartbeatInput{
		JobName:        opsCleanupJobName,
		LastRunAt:      &runAt,
		LastErrorAt:    &now,
		LastError:      &msg,
		LastDurationMs: &durMs,
	})
}

// Zero means usage history is indefinite, so its idempotency claims must also
// remain. Both YAML and the live request-history override are respected.
func (s *OpsCleanupService) billingDedupRetentionDays(ctx context.Context, effective opsCleanupEffectiveConfig) (int, error) {
	policy := &DashboardAggregationService{settingRepo: s.settingRepo}
	if s.cfg != nil {
		policy.cfg = s.cfg.DashboardAgg
	}
	usageDays, err := policy.requestRetentionDays(ctx)
	if err != nil || usageDays == 0 {
		return 0, err
	}
	return max(usageDays, max(policy.cfg.Retention.UsageBillingDedupDays,
		effective.BillingDedupHotRetentionDays+effective.BillingDedupArchiveRetentionDays)), nil
}
