package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *SiteStore) projectSiteSetting(raw []byte, install bool) ([]byte, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	var key, value string
	if err := json.Unmarshal(row["key"], &key); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(row["value"], &value); err != nil {
		return nil, err
	}
	if !service.IsSiteSetting(key) {
		return nil, fmt.Errorf("unsupported site setting %s", key)
	}
	if key != "web_search_emulation_config" && key != "ops_advanced_settings" {
		return raw, nil
	}
	if strings.TrimSpace(value) == "" {
		return raw, nil
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, fmt.Errorf("invalid site policy %s", key)
	}
	if obj == nil {
		return nil, fmt.Errorf("site setting %s must be an object", key)
	}
	if !install {
		switch key {
		case "web_search_emulation_config":
			clearSiteProxyReferences(obj)
		case "ops_advanced_settings":
			for k := range obj {
				if k != "data_retention" && !strings.HasPrefix(k, "ignore_") {
					delete(obj, k)
				}
			}
			if value := obj["data_retention"]; value != nil {
				retention, ok := value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid site retention policy")
				}
				// Preserve absence: synthesizing zero values could turn an older
				// partial setting into a zero-day deletion policy on the replica.
				allowed := map[string]bool{
					"cleanup_enabled": true, "cleanup_schedule": true,
					"error_log_retention_days": true, "minute_metrics_retention_days": true,
					"hourly_metrics_retention_days": true, "request_connection_retention_days": true,
					"request_timing_retention_days": true, "completed_billing_event_retention_days": true,
					"billing_dedup_hot_retention_days": true, "billing_dedup_archive_retention_days": true,
					"user_traffic_retention_days": true,
				}
				for name := range retention {
					if !allowed[name] {
						delete(retention, name)
					}
				}
			}
		}
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	row["value"], _ = json.Marshal(string(encoded))
	return json.Marshal(row)
}

func clearSiteProxyReferences(value any) {
	switch obj := value.(type) {
	case map[string]any:
		for k, v := range obj {
			if k == "proxy_id" {
				obj[k] = nil
			} else {
				clearSiteProxyReferences(v)
			}
		}
	case []any:
		for _, v := range obj {
			clearSiteProxyReferences(v)
		}
	}
}

func (s *SiteStore) installSiteSettings(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT value FROM site_replica_rows WHERE table_name='settings' ORDER BY row_key`)
	if err != nil {
		return err
	}
	var settings [][]byte
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		raw, err = s.projectSiteSetting(raw, true)
		if err != nil {
			_ = rows.Close()
			return err
		}
		settings = append(settings, raw)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, raw := range settings {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) SELECT key,value,updated_at FROM jsonb_populate_record(NULL::settings,$1::jsonb) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at WHERE settings.value IS DISTINCT FROM EXCLUDED.value`, string(raw))
		if err != nil {
			return err
		}
	}
	return nil
}
