package service

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

type siteRetentionCapture struct {
	calls  int
	policy SiteHistoryRetentionPolicy
}

func (c *siteRetentionCapture) PruneSiteHistory(_ context.Context, p SiteHistoryRetentionPolicy) error {
	c.calls++
	c.policy = p
	return nil
}

func TestSiteSecondaryCleanupRunsOnlyLocalPlan(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary}}
	cfg.DashboardAgg.Enabled = true
	cfg.DashboardAgg.Retention.UsageLogsDays = 90
	cfg.DashboardAgg.Retention.UsageBillingDedupDays = 365
	svc := NewOpsCleanupService(nil, db, cfg, nil, nil)
	svc.effective.Enabled = true
	svc.effective.ErrorLogRetentionDays = 30
	svc.maintenance = []opsCleanupMaintenance{{"global task", func(context.Context, time.Time) error { t.Fatal("secondary ran global maintenance"); return nil }}}
	local := &siteRetentionCapture{}
	svc.SetSiteHistoryRetention(local)
	_, err = svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, local.calls)
	require.WithinDuration(t, time.Now().AddDate(0, 0, -90), local.policy.Usage, time.Second)
	require.WithinDuration(t, time.Now().AddDate(0, 0, -365), local.policy.Dedup, time.Second)
	svc.effective.Enabled = false
	_, err = svc.runCleanupOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, local.calls)
	require.NoError(t, mock.ExpectationsWereMet(), "secondary must not run the primary SQL cleanup plan")
}
