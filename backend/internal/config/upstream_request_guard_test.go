package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadUpstreamRequestGuard(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, 120, cfg.Gateway.UpstreamUploadTimeoutSeconds)
		require.Equal(t, 30, cfg.Gateway.UpstreamClientDisconnectGraceSeconds)
	})
	for _, tc := range []struct {
		upload, drain         string
		wantUpload, wantDrain int
		invalid               string
	}{
		{"45", "10", 45, 10, ""}, {"0", "0", 0, 0, ""},
		{"-1", "10", 0, 0, "gateway.upstream_upload_timeout_seconds"},
		{"1801", "10", 0, 0, "gateway.upstream_upload_timeout_seconds"},
		{"60", "301", 0, 0, "gateway.upstream_client_disconnect_grace_seconds"},
		{"60", "-1", 0, 0, "gateway.upstream_client_disconnect_grace_seconds"},
	} {
		t.Run(tc.upload+"/"+tc.drain, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_UPSTREAM_UPLOAD_TIMEOUT_SECONDS", tc.upload)
			t.Setenv("GATEWAY_UPSTREAM_CLIENT_DISCONNECT_GRACE_SECONDS", tc.drain)
			cfg, err := Load()
			if tc.invalid != "" {
				require.ErrorContains(t, err, tc.invalid)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantUpload, cfg.Gateway.UpstreamUploadTimeoutSeconds)
			require.Equal(t, tc.wantDrain, cfg.Gateway.UpstreamClientDisconnectGraceSeconds)
		})
	}
}
