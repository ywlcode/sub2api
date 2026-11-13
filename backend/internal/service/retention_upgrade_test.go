package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBillingDedupNeverExpiresBeforeUsage(t *testing.T) {
	for _, tc := range []struct {
		name         string
		usage, dedup int
		runtime      string
		want         int
	}{
		{"legacy defaults", 90, 365, "", 365},
		{"long usage", 180, 30, "", 180},
		{"runtime extended", 90, 30, `{"request_retention_days":300}`, 300},
		{"indefinite", 90, 30, `{"request_retention_days":0}`, 0},
		{"explicit runtime shorter", 90, 365, `{"request_retention_days":7}`, 365},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.DashboardAgg.Enabled = true
			cfg.DashboardAgg.Retention.UsageLogsDays = tc.usage
			cfg.DashboardAgg.Retention.UsageBillingDedupDays = tc.dedup
			repo := newRuntimeSettingRepoStub()
			if tc.runtime != "" {
				require.NoError(t, repo.Set(context.Background(), SettingKeyOpsRuntimeLogConfig, tc.runtime))
			}
			s := &OpsCleanupService{cfg: cfg, settingRepo: repo}
			days, err := s.billingDedupRetentionDays(context.Background(), opsCleanupEffectiveConfig{BillingDedupHotRetentionDays: 15, BillingDedupArchiveRetentionDays: 15})
			require.NoError(t, err)
			require.Equal(t, tc.want, days)
		})
	}
}
