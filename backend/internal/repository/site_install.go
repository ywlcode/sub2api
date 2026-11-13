package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

// Installation touches changed identities and owners of unsettled bills, never
// every user/key simply because one bill is awaiting its authoritative receipt.
func (s *SiteStore) prepareSiteInstall(ctx context.Context, tx *sql.Tx, response site.SyncResponse) (map[string]bool, map[string]bool, []byte, bool, error) {
	dirty, changed := map[string]bool{}, map[string]bool{"$full": response.Full}
	allowed := map[string]siteTable{}
	for _, t := range s.tables {
		allowed[t.name] = t
		if response.Full {
			dirty[t.name] = true
		}
	}
	seen := map[string]bool{}
	for _, r := range response.Rows {
		if seen[r.Identity()] {
			return nil, nil, nil, false, errors.New("duplicate replica row")
		}
		seen[r.Identity()] = true
		if r.Table == "$quality" {
			if len(r.Key) != 64 {
				return nil, nil, nil, false, errors.New("invalid quality projection")
			}
			continue
		}
		if (r.Table == "$pricing" && r.Key == "catalog" || r.Table == "$policy" && r.Key == "request") && len(r.Value) > 0 {
			continue
		}
		t, ok := allowed[r.Table]
		if !ok {
			return nil, nil, nil, false, errors.New("unknown replica row")
		}
		if r.Table == "settings" && !service.IsSiteSetting(r.Key) {
			return nil, nil, nil, false, errors.New("setting outside site request policy")
		}
		if len(r.Value) > 0 {
			key, err := siteRowKey(t, r.Value)
			if err != nil || key != r.Key {
				return nil, nil, nil, false, errors.New("invalid replica row key")
			}
		}
		dirty[r.Table] = true
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE site_projection_stage(table_name text,row_key text,value jsonb,PRIMARY KEY(table_name,row_key)) ON COMMIT DROP`); err != nil {
		return nil, nil, nil, false, err
	}
	if err := s.stageProjection(ctx, tx, response.Rows); err != nil {
		return nil, nil, nil, false, err
	}
	// Remove only monetary fields when deciding whether configuration caches and
	// the model router need rebuilding. Status/permissions remain configuration.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT n.table_name FROM site_projection_stage n LEFT JOIN site_replica_rows old USING(table_name,row_key)
 CROSS JOIN LATERAL (SELECT CASE n.table_name
 WHEN 'users' THEN ARRAY['balance','updated_at']
 WHEN 'accounts' THEN ARRAY['updated_at','last_used_at','extra','ai_ops_revision']
 WHEN 'api_keys' THEN ARRAY['quota_used','usage_5h','usage_1d','usage_7d','window_5h_start','window_1d_start','window_7d_start','updated_at','last_used_at']
 WHEN 'user_subscriptions' THEN ARRAY['daily_usage_usd','weekly_usage_usd','monthly_usage_usd','daily_window_start','weekly_window_start','monthly_window_start','updated_at']
 ELSE ARRAY[]::text[] END AS ignored) f
 WHERE n.table_name<>'usage_platform_quota_totals' AND ((n.value-f.ignored) IS DISTINCT FROM (old.value-f.ignored)
 OR n.table_name='accounts' AND ((n.value->'extra')-ARRAY['quota_used','quota_daily_used','quota_daily_start','quota_daily_reset_at','quota_weekly_used','quota_weekly_start','quota_weekly_reset_at']) IS DISTINCT FROM ((old.value->'extra')-ARRAY['quota_used','quota_daily_used','quota_daily_start','quota_daily_reset_at','quota_weekly_used','quota_weekly_start','quota_weekly_reset_at']))`)
	if err != nil {
		return nil, nil, nil, false, err
	}
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			_ = rows.Close()
			return nil, nil, nil, false, err
		}
		changed[table] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, nil, nil, false, err
	}
	var previousAccounts []byte
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_object_agg(row_key,value),'{}'::jsonb) FROM site_replica_rows WHERE table_name='accounts'`).Scan(&previousAccounts); err != nil {
		return nil, nil, nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE site_install_keys(table_name text,row_key text,PRIMARY KEY(table_name,row_key)) ON COMMIT DROP`); err != nil {
		return nil, nil, nil, false, err
	}
	if response.Full {
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_install_keys SELECT table_name,row_key FROM site_replica_rows`); err != nil {
			return nil, nil, nil, false, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM site_replica_rows`); err != nil {
			return nil, nil, nil, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO site_install_keys SELECT table_name,row_key FROM site_projection_stage ON CONFLICT DO NOTHING`); err != nil {
		return nil, nil, nil, false, err
	}
	// Keep rejected bills overlaid too: quarantine is not a free refund. Operators
	// can reconcile durable payloads after fixing missing ownership/history.
	result, err := tx.ExecContext(ctx, `WITH bills AS (
 SELECT payload FROM site_outbox WHERE kind='billing' UNION ALL SELECT payload FROM site_rejected_events WHERE kind='billing'),
 owners AS (SELECT 'users' name,payload->'command'->>'UserID' key FROM bills
 UNION SELECT 'api_keys',payload->'command'->>'APIKeyID' FROM bills
 UNION SELECT 'accounts',payload->'command'->>'AccountID' FROM bills
 UNION SELECT 'user_subscriptions',payload->'command'->>'SubscriptionID' FROM bills
 UNION SELECT 'usage_platform_quota_totals',q.id::text FROM bills b JOIN user_platform_quotas q ON q.user_id=(b.payload->'command'->>'UserID')::bigint AND q.platform=b.payload->'command'->>'PlatformQuotaPlatform')
 INSERT INTO site_install_keys SELECT name,key FROM owners WHERE key IS NOT NULL ON CONFLICT DO NOTHING`)
	if err != nil {
		return nil, nil, nil, false, err
	}
	n, _ := result.RowsAffected()
	hadPending := n > 0
	if hadPending {
		for _, table := range []string{"users", "api_keys", "accounts", "user_subscriptions", "usage_platform_quota_totals"} {
			dirty[table] = true
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM site_replica_rows r USING site_projection_stage n WHERE r.table_name=n.table_name AND r.row_key=n.row_key AND n.value IS NULL`); err != nil {
		return nil, nil, nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO site_replica_rows SELECT table_name,row_key,value FROM site_projection_stage WHERE value IS NOT NULL ON CONFLICT(table_name,row_key) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		return nil, nil, nil, false, err
	}
	return dirty, changed, previousAccounts, hadPending, nil
}
