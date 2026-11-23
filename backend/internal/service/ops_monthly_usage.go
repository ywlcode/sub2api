package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/site"
)

// Both the billing query and the original Excel reports use Beijing calendar months.
var monthlyUsageZone = time.FixedZone("Asia/Shanghai", 8*60*60)

// MonthlyUsageDispatchCheckpoint tracks scheduled attempts, including durable rejections and unknown receipts.
const MonthlyUsageDispatchCheckpoint = "ops_monthly_usage_last_dispatched_period"
const monthlyUsageCheckpoint = MonthlyUsageDispatchCheckpoint

type MonthlyUsageDeliveryState string

const (
	MonthlyUsageUnclaimed MonthlyUsageDeliveryState = ""
	MonthlyUsageSending   MonthlyUsageDeliveryState = "sending"
	MonthlyUsageAccepted  MonthlyUsageDeliveryState = "accepted"
	MonthlyUsageRejected  MonthlyUsageDeliveryState = "rejected"
	MonthlyUsageUncertain MonthlyUsageDeliveryState = "uncertain"
)

// OpsMonthlyUsageRepository is available on authoritative sites for report generation and delivery receipts.
type OpsMonthlyUsageRepository interface {
	monthlyUsageWorkbookBuilder
	MonthlyUsageDepartmentAttributeAvailable(ctx context.Context, id int64) (bool, error)
	MonthlyUsageDelivery(context.Context, string, string) (MonthlyUsageDeliveryState, error)
	NextMonthlyUsageRetry(ctx context.Context, first, last string, recipients []string) (string, error)
	AdvanceMonthlyUsageDispatch(ctx context.Context, period, configValue string) (bool, error)
	ClaimMonthlyUsageEmailForConfig(ctx context.Context, month, recipient, configValue string) (bool, error)
	FinishMonthlyUsageEmail(context.Context, string, string, MonthlyUsageDeliveryState) error
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
		if err := s.opsService.opsRepo.UpsertJobHeartbeat(ctx, h); err != nil {
			log.Printf("[OpsMonthlyUsage] period=%s: heartbeat write failed: %v", period, err)
		}
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
	if err != nil {
		return "", err
	}
	retryOnly := period == ""
	if retryOnly {
		if last == "" || last < cfg.Report.MonthlyUsageFirstPeriod {
			return "", nil
		}
		period, err = store.NextMonthlyUsageRetry(ctx, cfg.Report.MonthlyUsageFirstPeriod, last, recipients)
		if err != nil || period == "" {
			return "", err
		}
	}

	processed, err := s.dispatchMonthlyUsage(ctx, store, cfg.Report, period, retryOnly, send)
	if err != nil {
		return period, err // Include the month in diagnostics even when dispatch could not advance.
	}
	return processed, nil
}

func (s *OpsScheduledReportService) dispatchMonthlyUsage(ctx context.Context, store OpsMonthlyUsageRepository, initial OpsEmailReportConfig, period string, retryOnly bool, send monthlyUsageSender) (string, error) {
	siteName := s.monthlyUsageSiteName(ctx)
	var file *bytes.Buffer
	var coverage string
	checked := make(map[string]bool)
	var sendErrors error
	var claimedAny bool
	for {
		// Reload after export, between individual messages, and before closing
		// the month, including the path where all addresses already have receipts.
		cfg, raw, err := s.opsService.readEmailNotificationConfig(ctx)
		if err != nil {
			return period, errors.Join(sendErrors, err)
		}
		recipients := normalizeEmails(cfg.Report.Recipients)
		if raw == nil || !s.opsService.IsMonitoringEnabled(ctx) || !cfg.Report.Enabled || !cfg.Report.MonthlyUsageEnabled ||
			cfg.Report.MonthlyUsageFirstPeriod != initial.MonthlyUsageFirstPeriod ||
			cfg.Report.MonthlyUsageDepartmentAttributeID != initial.MonthlyUsageDepartmentAttributeID || len(recipients) == 0 {
			return "", sendErrors
		}
		var next string
		for _, addr := range recipients {
			if checked[addr] {
				continue
			}
			state, err := store.MonthlyUsageDelivery(ctx, period, addr)
			if err != nil {
				return period, errors.Join(sendErrors, err)
			}
			if state == MonthlyUsageRejected || (state == MonthlyUsageUnclaimed && !retryOnly) {
				next = addr
				break
			}
			checked[addr] = true
		}
		if next == "" {
			if retryOnly && !claimedAny {
				return "", sendErrors // The rejection was claimed by a peer or its address was removed.
			}
			processed, err := closeMonthlyUsageDispatch(ctx, store, period, recipients, *raw, retryOnly)
			return processed, errors.Join(sendErrors, err)
		}
		if file == nil {
			file = new(bytes.Buffer)
			coverage, err = store.BuildMonthlyUsageReportWithCoverage(ctx, period, siteName, initial.MonthlyUsageDepartmentAttributeID, file)
			if err != nil {
				return period, fmt.Errorf("build monthly workbook: %w", err)
			}
			continue
		}
		checked[next] = true
		claimed, err := sendMonthlyUsageOnce(ctx, store, send, period, siteName, next, *raw, coverage, file.Bytes())
		claimedAny = claimedAny || claimed
		if err != nil {
			sendErrors = errors.Join(sendErrors, fmt.Errorf("recipient_hash=%s: %w", notificationEmailHash(next), err))
		}
	}
}

func closeMonthlyUsageDispatch(ctx context.Context, store OpsMonthlyUsageRepository, period string, recipients []string, configValue string, retryOnly bool) (string, error) {
	var rejected, uncertain int
	var incomplete bool
	for _, addr := range recipients {
		state, err := store.MonthlyUsageDelivery(ctx, period, addr)
		if err != nil {
			return period, err
		}
		switch state {
		case MonthlyUsageAccepted:
		case MonthlyUsageRejected:
			rejected++
		case MonthlyUsageUncertain:
			uncertain++
		case MonthlyUsageSending:
			incomplete = true
		case MonthlyUsageUnclaimed:
			// Retrying an old rejection never authorizes historical mail to a new address.
			if !retryOnly {
				incomplete = true
			}
		default:
			return period, fmt.Errorf("unexpected monthly delivery state %q", state)
		}
	}
	var outcome error
	if rejected > 0 || uncertain > 0 {
		outcome = fmt.Errorf("monthly report %s: %d rejected (queued for retry), %d uncertain (check mail logs)", period, rejected, uncertain)
	}
	if incomplete {
		return "", outcome
	}
	if !retryOnly {
		advanced, err := store.AdvanceMonthlyUsageDispatch(ctx, period, configValue)
		if err != nil || !advanced {
			return "", errors.Join(outcome, err)
		}
	}
	return period, outcome
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

func sendMonthlyUsageOnce(ctx context.Context, store OpsMonthlyUsageRepository, send monthlyUsageSender, month, siteName, to, configValue, coverage string, file []byte) (bool, error) {
	claimed, err := store.ClaimMonthlyUsageEmailForConfig(ctx, month, to, configValue)
	if err != nil || !claimed {
		return false, err
	}
	uncertain, err := sendMonthlyUsageAttachment(ctx, send, month, siteName, to, coverage, file)
	state := MonthlyUsageAccepted
	if uncertain {
		state = MonthlyUsageUncertain
		err = errors.Join(errors.New("monthly report SMTP receipt uncertain; check mail logs before resending"), err)
	} else if err != nil {
		state = MonthlyUsageRejected
	}
	// Until a definite rejection is durable, leave the claim in place. A DB
	// failure after SMTP (or a lost DB acknowledgment) is unsafe to replay.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if stateErr := store.FinishMonthlyUsageEmail(finalCtx, month, to, state); stateErr != nil {
		return true, errors.Join(err, fmt.Errorf("monthly report %s: SMTP %s; receipt update unconfirmed, check database and mail logs: %w", month, state, stateErr))
	}
	return true, err
}

func sendMonthlyUsageAttachment(ctx context.Context, send monthlyUsageSender, month, siteName, to, coverage string, file []byte) (bool, error) {
	body := fmt.Sprintf("%s · %s（北京时间）。本月报不含导出时为管理员的账号。明细和用户月汇总（Token 单位 M）涵盖所有平台。每个当月有请求记录的平台各有一张部门月汇总（Token 单位 B）；未记录平台的请求仅计入明细和用户月汇总。各汇总表的扣费均按汇总后取整数美元。", siteName, month)
	if coverage != "" {
		body += "\n" + coverage
	}
	return send(ctx, to, fmt.Sprintf("[Sub2API] %s · %s 月度用量报告", siteName, month), body,
		fmt.Sprintf("Sub2API-%s-%s.xlsx", siteName, month), file)
}
