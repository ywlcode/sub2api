package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestPromptCacheShadowConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		enabled         bool
	}{
		{"default", "", "", false},
		{"yaml", "gateway:\n  prompt_cache_shadow:\n    enabled: true\n    account_ids: [3, 7]\n", "", true},
		{"env", "", "true", true},
		{"env overrides yaml", "gateway:\n  prompt_cache_shadow:\n    enabled: true\n", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0600))
			t.Setenv("CONFIG_FILE", path)
			t.Setenv("GATEWAY_PROMPT_CACHE_SHADOW_ENABLED", tc.env)
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tc.enabled, cfg.Gateway.PromptCacheShadow.Enabled)
			require.Equal(t, 3600, cfg.Gateway.PromptCacheShadow.TTLSeconds)
			require.Equal(t, 8, cfg.Gateway.PromptCacheShadow.ComputeConcurrency)
			require.Equal(t, 1000000, cfg.Gateway.PromptCacheShadow.MaxEntries)
			require.Equal(t, 7, cfg.Gateway.PromptCacheShadow.LogRetentionDays)
			require.Equal(t, 5120, cfg.Gateway.PromptCacheShadow.LogMaxTotalMB)
			require.Equal(t, 1024, cfg.Gateway.PromptCacheShadow.LogQueueSize)
			if tc.name == "yaml" {
				require.Equal(t, []int64{3, 7}, cfg.Gateway.PromptCacheShadow.AccountIDs)
			}
		})
	}
}

func TestPromptCacheShadowNodeLocalAndLimits(t *testing.T) {
	p := &Config{}
	p.Gateway.PromptCacheShadow = PromptCacheShadowConfig{Enabled: true, AccountIDs: []int64{3}}
	raw, err := p.ExportSiteRequestPolicy()
	require.NoError(t, err)
	require.NotContains(t, string(raw), "prompt_cache_shadow")
	s := &Config{Site: site.Config{Mode: site.Secondary}}
	s.Gateway.PromptCacheShadow = PromptCacheShadowConfig{Enabled: true, AccountIDs: []int64{7}}
	require.NoError(t, s.InstallSiteRequestPolicy(raw))
	require.Equal(t, []int64{7}, s.RequestGateway().PromptCacheShadow.AccountIDs)
	for _, settings := range []PromptCacheShadowConfig{{TTLSeconds: -1}, {TTLSeconds: 86401}, {MaxEntries: 1000001}, {MaxBodyBytes: 8<<20 + 1}, {PrefixTokensStep: -1}, {AccountIDs: []int64{0}}, {LogDir: "relative/logs"}, {LogRetentionDays: 91}, {LogMaxTotalMB: -1}, {LogQueueSize: 16385}, {ComputeConcurrency: -1}, {ComputeConcurrency: 65}} {
		require.Error(t, settings.Validate())
	}
}

func TestPromptCacheShadowLogConfigOverrides(t *testing.T) {
	resetViperWithJWTSecret(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("gateway:\n  prompt_cache_shadow:\n    log_dir: /app/data/custom-shadow\n    log_retention_days: 14\n    log_max_total_mb: 2048\n    log_queue_size: 64\n"), 0600))
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("GATEWAY_PROMPT_CACHE_SHADOW_LOG_MAX_TOTAL_MB", "3072")
	t.Setenv("GATEWAY_PROMPT_CACHE_SHADOW_COMPUTE_CONCURRENCY", "12")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "/app/data/custom-shadow", cfg.Gateway.PromptCacheShadow.LogDir)
	require.Equal(t, 14, cfg.Gateway.PromptCacheShadow.LogRetentionDays)
	require.Equal(t, 3072, cfg.Gateway.PromptCacheShadow.LogMaxTotalMB)
	require.Equal(t, 64, cfg.Gateway.PromptCacheShadow.LogQueueSize)
	require.Equal(t, 12, cfg.Gateway.PromptCacheShadow.ComputeConcurrency)
}
