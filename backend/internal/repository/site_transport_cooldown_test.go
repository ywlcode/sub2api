package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteAccountTransportCooldownPreserved(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, override := range []string{"", "https://domestic.example"} {
			raw, err := json.Marshal(map[string]any{
				"id": 42, "type": "apikey", "platform": "deepseek", "proxy_id": 7,
				"extra": map[string]any{service.AccountExtraSecondaryEnabled: true},
				"credentials": map[string]any{
					"api_key": "test-key", "base_url": "https://primary-relay.example",
					service.UpstreamTransportCooldownCredentialKey: service.UpstreamTransportCooldownConfig{Enabled: enabled, DurationMinutes: 7},
				},
			})
			require.NoError(t, err)
			row, err := redactSiteRow("accounts", raw, site.Peer{BaseURLs: map[int64]string{42: override}})
			require.NoError(t, err)
			var account service.Account
			require.NoError(t, json.Unmarshal(row, &account))
			require.Nil(t, account.ProxyID)
			require.Equal(t, "test-key", account.Credentials["api_key"])
			if override != "" {
				require.Equal(t, override, account.Credentials["base_url"])
			}
			want := time.Duration(0)
			if enabled {
				want = 7 * time.Minute
			}
			require.Equal(t, want, account.UpstreamTransportCooldown(), "secondary must apply the configured policy after the JSON round trip")
		}
	}
}
