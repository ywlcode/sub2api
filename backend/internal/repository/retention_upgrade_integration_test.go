//go:build integration

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestRetentionUpgradePreservesExplicitSettings(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	migration, err := migrations.FS.ReadFile("340_preserve_retention_defaults.sql")
	require.NoError(t, err)
	for _, raw := range []string{`{}`, `{"data_retention":{"error_log_retention_days":120}}`, `{"data_retention":{"request_connection_retention_days":7,"request_timing_retention_days":9,"billing_dedup_hot_retention_days":10,"billing_dedup_archive_retention_days":20}}`, `invalid legacy JSON`} {
		_, err = db.Exec(`INSERT INTO settings(key,value) VALUES('ops_advanced_settings',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, raw)
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
		var got string
		require.NoError(t, db.QueryRow(`SELECT value FROM settings WHERE key='ops_advanced_settings'`).Scan(&got))
		if raw == "invalid legacy JSON" {
			require.Equal(t, raw, got)
			continue
		}
		var before, after map[string]map[string]int
		require.NoError(t, json.Unmarshal([]byte(raw), &before))
		require.NoError(t, json.Unmarshal([]byte(got), &after))
		for key, value := range before["data_retention"] {
			require.Equal(t, value, after["data_retention"][key])
		}
		for key, defaultValue := range map[string]int{"request_connection_retention_days": 30, "request_timing_retention_days": 30, "billing_dedup_hot_retention_days": 15, "billing_dedup_archive_retention_days": 350, "error_log_retention_days": 30} {
			if _, explicit := before["data_retention"][key]; !explicit {
				require.Equal(t, defaultValue, after["data_retention"][key])
			}
		}
	}
}
