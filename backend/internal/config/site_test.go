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
	t.Setenv("SITE_FORWARD_UPLOAD_IDLE_TIMEOUT_SECONDS", "42")
	t.Setenv("SITE_FORWARD_RESPONSE_HEADER_TIMEOUT_SECONDS", "0")
	t.Setenv("SITE_FORWARD_RESPONSE_IDLE_TIMEOUT_SECONDS", "300")
	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.IsSecondary())
	require.NotNil(t, cfg.Site.ForwardUploadIdleTimeoutSeconds)
	require.Equal(t, 42, *cfg.Site.ForwardUploadIdleTimeoutSeconds)
	require.NotNil(t, cfg.Site.ForwardResponseHeaderTimeoutSeconds)
	require.Zero(t, *cfg.Site.ForwardResponseHeaderTimeoutSeconds)
	require.NotNil(t, cfg.Site.ForwardResponseIdleTimeoutSeconds)
	require.Equal(t, 300, *cfg.Site.ForwardResponseIdleTimeoutSeconds)
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
