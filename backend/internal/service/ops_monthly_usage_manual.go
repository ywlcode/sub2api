package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
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

func (s *OpsService) StartManualMonthlyUsage(ctx context.Context, month, id string) (*OpsMonthlyUsageJob, error) {
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

	cfg, err := s.GetEmailNotificationConfig(ctx)
	if err != nil {
		return nil, err
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
	ledger, ok := s.opsRepo.(OpsMonthlyUsageRepository)
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
	job, created, err := store.BeginManualMonthlyUsage(ctx, id, month, len(recipients))
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

func (s *OpsScheduledReportService) runManualMonthlyUsage(ctx context.Context, store OpsManualMonthlyUsageRepository, ledger OpsMonthlyUsageRepository, job OpsMonthlyUsageJob, recipients []string, departmentAttributeID int64, send monthlyUsageSender) {
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
	if err := ledger.BuildMonthlyUsageReport(ctx, job.Month, label, departmentAttributeID, &file); err != nil {
		log.Printf("[OpsMonthlyUsage] manual job %s export: %v", job.ID, err)
		job.Status, job.Failed = "failed", job.Total
		save()
		return
	}
	// Allow sending only to the intersection of the originally approved and
	// currently saved recipients. Manual jobs never alter the automatic receipts
	// or checkpoint; reconnecting with the same request ID never replays mail.
	cfg, err := s.opsService.GetEmailNotificationConfig(ctx)
	allowed := map[string]bool{}
	if err == nil && cfg.Report.Enabled && cfg.Report.MonthlyUsageDepartmentAttributeID == departmentAttributeID && s.opsService.IsMonitoringEnabled(ctx) {
		for _, addr := range normalizeEmails(cfg.Report.Recipients) {
			allowed[addr] = true
		}
	}
	for i, addr := range recipients {
		if ctx.Err() != nil {
			job.Failed += len(recipients) - i
			break
		}
		if !allowed[addr] {
			job.Failed++
		} else {
			unknown, sendErr := sendMonthlyUsageAttachment(ctx, send, job.Month, label, addr, file.Bytes())
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
