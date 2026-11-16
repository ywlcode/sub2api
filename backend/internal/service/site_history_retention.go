package service

import (
	"context"
	"time"
)

// Zero cutoffs disable deletion. Site retention shares the existing daily owner;
// replicas only clean local histories, never the control plane or unsettled money.
type SiteHistoryRetentionPolicy struct {
	ErrorLogs, SystemLogs, Traces, Timings time.Time
	Usage, BillingEvents, Dedup, Traffic   time.Time
}

type SiteHistoryRetention interface {
	PruneSiteHistory(context.Context, SiteHistoryRetentionPolicy) error
}

func (s *OpsCleanupService) SetSiteHistoryRetention(retention SiteHistoryRetention) {
	s.mu.Lock()
	s.siteHistory = retention
	s.mu.Unlock()
}

func (s *OpsCleanupService) siteHistoryPolicy(ctx context.Context, now time.Time, effective opsCleanupEffectiveConfig) (SiteHistoryRetentionPolicy, error) {
	cutoff := func(days int) time.Time {
		if days < 0 {
			return time.Time{}
		}
		return now.AddDate(0, 0, -days)
	}
	p := SiteHistoryRetentionPolicy{
		ErrorLogs: cutoff(effective.ErrorLogRetentionDays), SystemLogs: cutoff(effective.SystemLogRetentionDays),
		Traces: cutoff(effective.RequestConnectionRetentionDays), Timings: cutoff(effective.RequestTimingRetentionDays),
		BillingEvents: cutoff(effective.CompletedBillingEventRetentionDays), Traffic: cutoff(effective.UserTrafficRetentionDays),
	}
	policy := &DashboardAggregationService{settingRepo: s.settingRepo, cfg: s.cfg.DashboardAgg}
	days, err := policy.requestRetentionDays(ctx)
	if err != nil {
		return p, err
	}
	if days > 0 {
		p.Usage = cutoff(days)
	}
	days, err = s.billingDedupRetentionDays(ctx, effective)
	if err != nil {
		return p, err
	}
	if days > 0 {
		p.Dedup = cutoff(days)
	}
	return p, nil
}
