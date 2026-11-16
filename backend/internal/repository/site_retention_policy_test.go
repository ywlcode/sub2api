package repository

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSiteReviewRetentionSettingsReachReplicaWithoutOperationalSecrets(t *testing.T) {
	value := `{"data_retention":{"cleanup_enabled":false,"cleanup_schedule":"0 3 * * *","request_timing_retention_days":45,"future_unreviewed_field":"private"},"ignore_client_errors":true,"email_notification":{"password":"private"},"aggregation":{"enabled":true}}`
	raw, err := json.Marshal(map[string]string{"key": "ops_advanced_settings", "value": value})
	require.NoError(t, err)
	projected, err := (&SiteStore{}).projectSiteSetting(raw, false)
	require.NoError(t, err)
	var row map[string]string
	require.NoError(t, json.Unmarshal(projected, &row))
	var settings map[string]any
	require.NoError(t, json.Unmarshal([]byte(row["value"]), &settings))
	require.Contains(t, settings, "data_retention")
	retention, ok := settings["data_retention"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, retention["cleanup_enabled"])
	require.Equal(t, float64(45), retention["request_timing_retention_days"])
	require.NotContains(t, settings["data_retention"], "error_log_retention_days", "missing settings must retain defaults, not turn into zero-day deletion")
	require.NotContains(t, string(projected), "private")
	require.NotContains(t, settings, "aggregation")
}
