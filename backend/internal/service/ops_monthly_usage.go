package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/site"
)

// Both the billing query and the original Excel reports use Beijing calendar months.
var monthlyUsageZone = time.FixedZone("Asia/Shanghai", 8*60*60)

const monthlyUsageCheckpoint = "ops_monthly_usage_last_completed_period"

// OpsMonthlyUsageRepository is available on authoritative sites for report generation and delivery receipts.
type OpsMonthlyUsageRepository interface {
	BuildMonthlyUsageReport(ctx context.Context, month, site string, departmentAttributeID int64, w io.Writer) error
	MonthlyUsageDepartmentAttributeAvailable(ctx context.Context, id int64) (bool, error)
	MonthlyUsageEmailClaimed(context.Context, string, string) (bool, error)
	MonthlyUsageEmailSettled(context.Context, string, string) (bool, error)
	ClaimMonthlyUsageEmail(context.Context, string, string) (bool, error)
	FinishMonthlyUsageEmail(context.Context, string, string, bool) error
}

type monthlyUsageSender func(context.Context, string, string, string, string, []byte) (bool, error)

func firstMonthlyUsagePeriod(now time.Time) string {
	t := now.In(monthlyUsageZone)
	if t.Day() == 1 {
		return t.AddDate(0, -1, 0).Format("2006-01")
	}
	return t.Format("2006-01")
}

func dueMonthlyUsagePeriod(now time.Time, first, last string) (string, error) {
	start, err := time.Parse("2006-01", first)
	if err != nil {
		return "", fmt.Errorf("invalid monthly report start period: %w", err)
	}
	if last != "" {
		t, err := time.Parse("2006-01", last)
		if err != nil {
			return "", fmt.Errorf("invalid monthly report checkpoint: %w", err)
		}
		if next := t.AddDate(0, 1, 0); next.After(start) {
			start = next
		}
	}
	due := time.Date(start.Year(), start.Month()+1, 1, 6, 15, 0, 0, monthlyUsageZone)
	if now.Before(due) {
		return "", nil
	}
	return start.Format("2006-01"), nil
}

// Use a separate worker: an export can exceed the ordinary reports' one-minute
// context, and must not prevent a daily/weekly report from firing on time.
func (s *OpsScheduledReportService) runMonthlyUsage() {
	defer s.wg.Done()
	for s.stopCtx.Err() == nil {
		now := time.Now()
		ctx, cancel := context.WithTimeout(s.stopCtx, 15*time.Minute)
		period, err := s.monthlyUsageTick(ctx, now, s.emailService.SendMonthlyUsageExcel)
		cancel()
		if err != nil || period != "" {
			s.monthlyUsageHeartbeat(now, period, err)
		}

		// First check on startup catches downtime; hourly checks at :15 also
		// retry explicit SMTP failures without a minute-by-minute mail storm.
		now = time.Now().In(monthlyUsageZone)
		next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 15, 0, 0, monthlyUsageZone)
		if !next.After(now) {
			next = next.Add(time.Hour)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-s.stopCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *OpsScheduledReportService) monthlyUsageHeartbeat(started time.Time, period string, err error) {
	now := time.Now().UTC()
	ms := time.Since(started).Milliseconds()
	h := &OpsUpsertJobHeartbeatInput{JobName: "ops_monthly_usage_report", LastRunAt: &started, LastDurationMs: &ms}
	if err != nil {
		msg := truncateString(err.Error(), 2048)
		h.LastErrorAt, h.LastError = &now, &msg
		log.Printf("[OpsMonthlyUsage] period=%s: %v", period, err)
	} else {
		msg := "processed report " + period
		h.LastSuccessAt, h.LastResult = &now, &msg
	}
	if s.opsService.opsRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.opsService.opsRepo.UpsertJobHeartbeat(ctx, h)
	}
}

func (s *OpsScheduledReportService) monthlyUsageTick(ctx context.Context, now time.Time, send monthlyUsageSender) (string, error) {
	if !s.cfg.RunsBackgroundJobs() || s.opsService == nil || !s.opsService.IsMonitoringEnabled(ctx) {
		return "", nil
	}
	cfg, err := s.opsService.GetEmailNotificationConfig(ctx)
	if err != nil || !cfg.Report.Enabled || !cfg.Report.MonthlyUsageEnabled {
		return "", err
	}
	recipients := normalizeEmails(cfg.Report.Recipients)
	if len(recipients) == 0 {
		return "", nil // No fallback to an admin or the separate alert recipient list.
	}
	store, ok := s.opsService.opsRepo.(OpsMonthlyUsageRepository)
	if !ok || s.opsService.settingRepo == nil {
		return "", errors.New("monthly usage repository not initialized")
	}
	last, err := s.opsService.settingRepo.GetValue(ctx, monthlyUsageCheckpoint)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return "", err
	}
	period, err := dueMonthlyUsagePeriod(now, cfg.Report.MonthlyUsageFirstPeriod, last)
	if err != nil || period == "" {
		return "", err
	}
	siteName := s.monthlyUsageSiteName(ctx)

	// Check cheaply before aggregating an entire month's ledger. Claim only
	// after export, immediately before SMTP, never while building the report.
	var pending []string
	for _, addr := range recipients {
		exists, err := store.MonthlyUsageEmailClaimed(ctx, period, addr)
		if err != nil {
			return period, err
		}
		if !exists {
			pending = append(pending, addr)
		}
	}
	if len(pending) > 0 {
		var file bytes.Buffer
		if err := store.BuildMonthlyUsageReport(ctx, period, siteName, cfg.Report.MonthlyUsageDepartmentAttributeID, &file); err != nil {
			return period, fmt.Errorf("build monthly workbook: %w", err)
		}
		// Respect a changed switch or recipient list while the read-only scan was running.
		current, err := s.opsService.GetEmailNotificationConfig(ctx)
		if err != nil || !s.opsService.IsMonitoringEnabled(ctx) || !current.Report.Enabled ||
			!current.Report.MonthlyUsageEnabled || current.Report.MonthlyUsageFirstPeriod != cfg.Report.MonthlyUsageFirstPeriod ||
			current.Report.MonthlyUsageDepartmentAttributeID != cfg.Report.MonthlyUsageDepartmentAttributeID {
			return "", err
		}
		allowed := make(map[string]bool)
		for _, addr := range normalizeEmails(current.Report.Recipients) {
			allowed[addr] = true
		}
		var failures []error
		for _, addr := range pending {
			if !allowed[addr] {
				continue
			}
			if err := sendMonthlyUsageOnce(ctx, store, send, period, siteName, addr, file.Bytes()); err != nil {
				failures = append(failures, err)
			}
		}
		if err := errors.Join(failures...); err != nil {
			return period, err
		}
		recipients = normalizeEmails(current.Report.Recipients)
	}
	// A peer may currently own a claim and still receive an explicit SMTP
	// rejection. Its mere existence is not enough to advance the whole month.
	for _, addr := range recipients {
		settled, err := store.MonthlyUsageEmailSettled(ctx, period, addr)
		if err != nil || !settled {
			return "", err
		}
	}
	return period, s.opsService.settingRepo.Set(ctx, monthlyUsageCheckpoint, period)
}

func (s *OpsScheduledReportService) monthlyUsageSiteName(ctx context.Context) string {
	siteName := "独立站"
	if s.cfg != nil && s.cfg.Site.EffectiveMode() == site.Primary {
		siteName = "主站"
	}
	if name, _ := s.opsService.settingRepo.GetValue(ctx, SettingKeySiteName); strings.TrimSpace(name) != "" {
		// A site name may contain path separators or control characters.
		clean := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
				return '-'
			}
			return r
		}, name))
		runes := []rune(clean)
		siteName += "-" + string(runes[:min(len(runes), 64)])
	}

	return siteName
}

func sendMonthlyUsageOnce(ctx context.Context, store OpsMonthlyUsageRepository, send monthlyUsageSender, month, siteName, to string, file []byte) error {
	claimed, err := store.ClaimMonthlyUsageEmail(ctx, month, to)
	if err != nil || !claimed {
		return err
	}
	uncertain, err := sendMonthlyUsageAttachment(ctx, send, month, siteName, to, file)
	if uncertain {
		return errors.New("monthly report SMTP receipt uncertain; check mail logs before resending")
	}
	// Complete or release even if the worker was canceled during SMTP.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stateErr := store.FinishMonthlyUsageEmail(finalCtx, month, to, err == nil)
	return errors.Join(err, stateErr)
}

func sendMonthlyUsageAttachment(ctx context.Context, send monthlyUsageSender, month, siteName, to string, file []byte) (bool, error) {
	return send(ctx, to, fmt.Sprintf("[Sub2API] %s · %s 月度用量报告", siteName, month),
		fmt.Sprintf("%s · %s（北京时间）。本月报不含导出时为管理员的账号。附件包含明细、部门月汇总（Token 单位 B）和用户月汇总（Token 单位 M）。两张汇总表的扣费均按汇总后取整数美元。", siteName, month),
		fmt.Sprintf("Sub2API-%s-%s.xlsx", siteName, month), file)
}
