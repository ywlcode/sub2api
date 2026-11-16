package server

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

type testKeyRiskStarter struct{ started, configured, stopped bool }

func (s *testKeyRiskStarter) SetKeyRiskAPIKeys(*service.APIKeyService) { s.configured = true }
func (s *testKeyRiskStarter) StartKeyRisk(string) (func(), error) {
	s.started = true
	return func() { s.stopped = true }, nil
}

func TestSiteReviewKeyRiskStartup(t *testing.T) {
	for _, tc := range []struct {
		mode, role string
		start      bool
	}{
		{site.Standalone, config.RuntimeRoleFull, true},
		{site.Primary, config.RuntimeRoleFull, true},
		{site.Secondary, config.RuntimeRoleFull, false},
		{site.Standalone, config.RuntimeRoleGateway, false},
	} {
		t.Run(tc.mode+"/"+tc.role, func(t *testing.T) {
			cfg := &config.Config{Site: site.Config{Mode: tc.mode}}
			cfg.Runtime.Role = tc.role
			starter, lifecycle := &testKeyRiskStarter{}, &Lifecycle{}
			configureKeyRisk(cfg, starter, nil, lifecycle)
			require.Equal(t, tc.start, starter.started, "replica must not open the keyrisk database or scan/send mail")
			require.Equal(t, tc.start, starter.configured)
			if tc.start {
				require.Len(t, lifecycle.onDrain, 1)
				lifecycle.onDrain[0]()
				require.True(t, starter.stopped)
			} else {
				require.Empty(t, lifecycle.onDrain)
			}
		})
	}
}
