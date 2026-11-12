package config

import (
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestSiteModesAndEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SITE_MODE", "secondary")
	t.Setenv("SITE_ID", "office")
	t.Setenv("SITE_PRIMARY_URL", "https://primary.example")
	t.Setenv("SITE_TOKEN", strings.Repeat("s", 32))
	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.IsSecondary())
	require.False(t, cfg.RunsBackgroundJobs())
	require.False(t, cfg.WritesSchedulerSnapshots())
	cfg.Runtime.Role = RuntimeRoleGateway
	require.Error(t, cfg.validateRuntime())
	cfg.Runtime.Role = RuntimeRoleFull
	cfg.RunMode = RunModeSimple
	require.Error(t, cfg.validateRuntime())
	for _, mode := range []string{"", site.Standalone, site.Primary} {
		cfg := &Config{Site: site.Config{Mode: mode, ID: "bosc"}}
		require.NoError(t, cfg.validateRuntime())
		require.True(t, cfg.RunsBackgroundJobs())
	}
}
