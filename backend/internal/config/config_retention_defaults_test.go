package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDedupRetentionDefaultWithLegacyUsageWindow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yaml      string
		usageEnv  string
		dedupEnv  string
		wantUsage int
		wantDedup int
		wantError bool
	}{
		{name: "new installation", yaml: "{}", wantUsage: 30, wantDedup: 30},
		{name: "legacy compose environment", yaml: "{}", usageEnv: "90", wantUsage: 90, wantDedup: 90},
		{name: "legacy yaml", yaml: "dashboard_aggregation:\n  retention:\n    usage_logs_days: 120\n", wantUsage: 120, wantDedup: 120},
		{name: "short usage window", yaml: "{}", usageEnv: "7", wantUsage: 7, wantDedup: 30},
		{name: "explicit env retained", yaml: "{}", usageEnv: "90", dedupEnv: "365", wantUsage: 90, wantDedup: 365},
		{name: "explicit yaml retained", yaml: "dashboard_aggregation:\n  retention:\n    usage_logs_days: 90\n    usage_billing_dedup_days: 365\n", wantUsage: 90, wantDedup: 365},
		{name: "invalid explicit env rejected", yaml: "{}", usageEnv: "90", dedupEnv: "30", wantError: true},
		{name: "invalid explicit yaml rejected", yaml: "dashboard_aggregation:\n  retention:\n    usage_logs_days: 90\n    usage_billing_dedup_days: 30\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0600))
			t.Setenv("CONFIG_FILE", path)
			t.Setenv("DASHBOARD_AGGREGATION_RETENTION_USAGE_LOGS_DAYS", tc.usageEnv)
			t.Setenv("DASHBOARD_AGGREGATION_RETENTION_USAGE_BILLING_DEDUP_DAYS", tc.dedupEnv)
			cfg, err := Load()
			if tc.wantError {
				require.ErrorContains(t, err, "usage_billing_dedup_days must be greater than or equal to usage_logs_days")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantUsage, cfg.DashboardAgg.Retention.UsageLogsDays)
			require.Equal(t, tc.wantDedup, cfg.DashboardAgg.Retention.UsageBillingDedupDays)
		})
	}
}
