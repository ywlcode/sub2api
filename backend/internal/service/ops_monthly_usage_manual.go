package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"time"

	"github.com/google/uuid"
)

var (
	ErrMonthlyUsageInvalid     = errors.New("invalid monthly report request")
	ErrMonthlyUsageBusy        = errors.New("a monthly report is already being sent")
	ErrMonthlyUsageConflict    = errors.New("monthly report request ID belongs to a different month")
	ErrMonthlyUsageJobNotFound = errors.New("monthly report job not found")
)

type OpsMonthlyUsageJob struct {
	ID        string `json:"id"`
	Month     string `json:"month"`
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Accepted  int    `json:"accepted"`
	Failed    int    `json:"failed"`
	Uncertain int    `json:"uncertain"`
}

type OpsManualMonthlyUsageRepository interface {
	BeginManualMonthlyUsage(context.Context, string, string, int) (*OpsMonthlyUsageJob, bool, error)
	GetManualMonthlyUsage(context.Context, string) (*OpsMonthlyUsageJob, error)
	UpdateManualMonthlyUsage(context.Context, *OpsMonthlyUsageJob) error
}

type monthlyUsageWorkbookBuilder interface {
	BuildMonthlyUsageReportWithCoverage(context.Context, string, string, int64, io.Writer) (string, error)
}

func canonicalMonthlyJobID(raw string) (string, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || id.String() != raw {
		return "", fmt.Errorf("%w: expected a canonical request UUID", ErrMonthlyUsageInvalid)
	}
	return id.String(), nil
}

func validateManualMonthlyPeriod(month string, now time.Time) error {
	t, err := time.Parse("2006-01", month)
	if err != nil || t.Year() < 1 || month >= now.In(monthlyUsageZone).Format("2006-01") {
		return fmt.Errorf("%w: select a completed Beijing calendar month (YYYY-MM)", ErrMonthlyUsageInvalid)
	}
	return nil
}

func (s *OpsService) manualMonthlyStore(ctx context.Context) (OpsManualMonthlyUsageRepository, error) {
	if s == nil || !s.cfg.RunsBackgroundJobs() || !s.IsMonitoringEnabled(ctx) {
		return nil, fmt.Errorf("%w: monitoring is unavailable on this site", ErrMonthlyUsageInvalid)
	}
	store, ok := s.opsRepo.(OpsManualMonthlyUsageRepository)
	if !ok {
		return nil, errors.New("manual monthly report repository not initialized")
	}
	return store, nil
}

func (s *OpsService) ManualMonthlyUsageStatus(ctx context.Context, id string) (*OpsMonthlyUsageJob, error) {
	if _, err := canonicalMonthlyJobID(id); err != nil {
		return nil, err
	}
	store, err := s.manualMonthlyStore(ctx)
	if err != nil {
		return nil, err
	}
	return store.GetManualMonthlyUsage(ctx, id)
}

func (s *OpsService) StartManualMonthlyUsage(ctx context.Context, month, id string, expectedRevision ...string) (*OpsMonthlyUsageJob, error) {
	if err := validateManualMonthlyPeriod(month, time.Now()); err != nil {
		return nil, err
	}
	if _, err := canonicalMonthlyJobID(id); err != nil {
		return nil, err
	}
	store, err := s.manualMonthlyStore(ctx)
	if err != nil {
		return nil, err
	}
	if job, err := store.GetManualMonthlyUsage(ctx, id); err == nil {
		if job.Month != month {
			return nil, ErrMonthlyUsageConflict
		}
		return job, nil
	} else if !errors.Is(err, ErrMonthlyUsageJobNotFound) {
		return nil, err
	}

	if len(expectedRevision) != 1 || expectedRevision[0] == "" {
		return nil, ErrReportRevisionRequired
	}
	cfg, err := s.GetEmailNotificationConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.ReportRevision != expectedRevision[0] {
		return nil, ErrReportRevisionConflict
	}
	recipients := normalizeEmails(cfg.Report.Recipients)
	if !cfg.Report.Enabled || len(recipients) == 0 {
		return nil, fmt.Errorf("%w: enable assessment reports and save at least one report recipient", ErrMonthlyUsageInvalid)
	}
	for _, recipient := range recipients {
		addr, err := parseSMTPAddress(recipient, "recipient")
		if err != nil || addr.Address != recipient {
			return nil, fmt.Errorf("%w: invalid saved report recipient", ErrMonthlyUsageInvalid)
		}
	}
	worker := s.monthlyReports
	ledger, ok := s.opsRepo.(monthlyUsageWorkbookBuilder)
	if worker == nil || worker.emailService == nil || !ok {
		return nil, errors.New("monthly report worker not initialized")
	}
	// Catch a missing SMTP configuration before starting an expensive export.
	if _, err := worker.emailService.GetSMTPConfig(ctx); err != nil {
		return nil, err
	}
	// Reserve a worker slot before claiming. Stop waits for admitted submissions
	// and their workers, but neither a browser disconnect nor closing the settings
	// dialog cancels a job once accepted.
	worker.manualMu.Lock()
	if !worker.manualOpen || worker.stopCtx.Err() != nil {
		worker.manualMu.Unlock()
		return nil, errors.New("monthly report worker is stopping")
	}
	worker.wg.Add(1)
	base := worker.stopCtx
	worker.manualMu.Unlock()
	// The INSERT may already be committed when the HTTP connection is lost.
	// Give the admitted worker its own bounded context to receive the result.
	claimCtx, cancelClaim := context.WithTimeout(context.WithoutCancel(base), 10*time.Second)
	job, created, err := store.BeginManualMonthlyUsage(claimCtx, id, month, len(recipients))
	cancelClaim()
	if err != nil || !created {
		worker.wg.Done()
		return job, err
	}
	// Never mutate the instance concurrently serialized by the HTTP response.
	go func(state OpsMonthlyUsageJob) {
		defer worker.wg.Done()
		workCtx, cancel := context.WithTimeout(base, 15*time.Minute)
		defer cancel()
		worker.runManualMonthlyUsage(workCtx, store, ledger, state, recipients, cfg.Report.MonthlyUsageDepartmentAttributeID, worker.emailService.SendMonthlyUsageExcel)
	}(*job)
	return job, nil
}

func (s *OpsScheduledReportService) runManualMonthlyUsage(ctx context.Context, store OpsManualMonthlyUsageRepository, ledger monthlyUsageWorkbookBuilder, job OpsMonthlyUsageJob, recipients []string, departmentAttributeID int64, send monthlyUsageSender) {
	save := func() bool {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.UpdateManualMonthlyUsage(persist, &job); err != nil {
			log.Printf("[OpsMonthlyUsage] manual job %s updating state: %v", job.ID, err)
			return false
		}
		return true
	}
	var file bytes.Buffer
	label := s.monthlyUsageSiteName(ctx)
	coverage, err := ledger.BuildMonthlyUsageReportWithCoverage(ctx, job.Month, label, departmentAttributeID, &file)
	if err != nil {
		log.Printf("[OpsMonthlyUsage] manual job %s export: %v", job.ID, err)
		job.Status, job.Failed = "failed", job.Total
		save()
		return
	}
	// Allow sending only to the intersection of the originally approved and
	// currently saved recipients. Manual jobs never alter the automatic receipts
	// or checkpoint; reconnecting with the same request ID never replays mail.
	for i, addr := range recipients {
		if ctx.Err() != nil {
			job.Failed += len(recipients) - i
			break
		}
		if !s.opsService.manualMonthlyRecipientAllowed(ctx, addr, departmentAttributeID) {
			job.Failed++
		} else {
			unknown, sendErr := sendMonthlyUsageAttachment(ctx, send, job.Month, label, addr, coverage, file.Bytes())
			switch {
			case unknown:
				job.Uncertain++
			case sendErr != nil:
				job.Failed++
			default:
				job.Accepted++
			}
		}
		if i+1 < len(recipients) && !save() {
			return
		}
	}
	switch {
	case job.Uncertain > 0:
		job.Status = "uncertain"
	case job.Failed > 0:
		job.Status = "failed"
	default:
		job.Status = "sent"
	}
	save()
}

// Recheck the DB-backed monitoring switch and the report config together at each
// SMTP handoff. Another administrator can revoke delivery while a job is running.
func (s *OpsService) manualMonthlyRecipientAllowed(ctx context.Context, recipient string, departmentID int64) bool {
	if !s.IsMonitoringEnabled(ctx) || s.settingRepo == nil {
		return false
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyOpsMonitoringEnabled, SettingKeyOpsEmailNotificationConfig})
	if err != nil || !parseOpsMonitoringEnabled(values[SettingKeyOpsMonitoringEnabled]) {
		return false
	}
	var cfg OpsEmailNotificationConfig
	if json.Unmarshal([]byte(values[SettingKeyOpsEmailNotificationConfig]), &cfg) != nil {
		return false
	}
	return cfg.Report.Enabled && cfg.Report.MonthlyUsageDepartmentAttributeID == departmentID &&
		slices.Contains(normalizeEmails(cfg.Report.Recipients), recipient)
}
