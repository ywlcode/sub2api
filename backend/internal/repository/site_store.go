package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/lib/pq"
)

const siteFence = 73910261112

// The account switch is the sole credential grant; pairing only identifies a node.
const siteAccountGrantSQL = `deleted_at IS NULL AND type='apikey' AND parent_account_id IS NULL AND extra->'site_secondary_enabled'='true'::jsonb`

var siteSettingKeysSQL = func() string {
	keys := make([]string, len(service.SiteSettingKeys))
	for i, key := range service.SiteSettingKeys {
		keys[i] = pq.QuoteLiteral(key)
	}
	return strings.Join(keys, ",")
}()

func siteSettingsPredicate() string {
	return "key IN (" + siteSettingKeysSQL + ")"
}

type siteTable struct {
	name    string
	keys    []string
	filter  string
	columns []string
}

// This is a deliberately closed list of gateway policy tables, not database
// replication. Passwords, login sessions, payments and credentials of ungranted accounts
// are never exported. All tables share one repeatable-read snapshot.
func siteTables() []siteTable {
	return []siteTable{
		{name: "users", keys: []string{"id"}},
		{name: "groups", keys: []string{"id"}},
		{name: "channels", keys: []string{"id"}},
		{name: "channel_groups", keys: []string{"id"}},
		{name: "channel_model_pricing", keys: []string{"id"}},
		{name: "channel_pricing_intervals", keys: []string{"id"}},
		{name: "channel_account_stats_pricing_rules", keys: []string{"id"}},
		{name: "channel_account_stats_model_pricing", keys: []string{"id"}},
		{name: "channel_account_stats_pricing_intervals", keys: []string{"id"}},
		{name: "tls_fingerprint_profiles", keys: []string{"id"}, filter: "id IN (SELECT (extra->>'tls_fingerprint_profile_id')::bigint FROM accounts WHERE id IN (%[1]s) AND extra->>'tls_fingerprint_profile_id' ~ '^[0-9]+$') OR EXISTS(SELECT 1 FROM accounts WHERE id IN (%[1]s) AND extra->>'tls_fingerprint_profile_id'='-1')"},
		{name: "accounts", keys: []string{"id"}, filter: "id IN (%s)"},
		{name: "account_groups", keys: []string{"account_id", "group_id"}, filter: "account_id IN (%s)"},
		{name: "api_keys", keys: []string{"id"}},
		{name: "api_key_groups", keys: []string{"api_key_id", "group_id"}},
		{name: "user_allowed_groups", keys: []string{"user_id", "group_id"}},
		{name: "user_group_rate_multipliers", keys: []string{"user_id", "group_id"}},
		{name: "user_subscriptions", keys: []string{"id"}},
		{name: "user_platform_quotas", keys: []string{"id"}},
		{name: "usage_platform_quota_totals", keys: []string{"quota_id"}},
		{name: "composite_model_routes", keys: []string{"id"}},
		{name: "error_passthrough_rules", keys: []string{"id"}},
		{name: "settings", keys: []string{"key"}},
	}
}

type SiteStore struct {
	db                   *sql.DB
	cfg                  site.Config
	tables               []siteTable
	schema               string
	AfterInstall         func(context.Context) error
	AfterPolicyInstall   func(context.Context, map[string]bool) error
	MapChannelModel      func(int64, string) string
	AfterImport          func(context.Context, []service.UsageBillingEvent) error
	ExportPricing        func() ([]byte, error)
	InstallPricing       func([]byte) error
	ExportRequestPolicy  func() ([]byte, error)
	InstallRequestPolicy func([]byte) error
	modelRouter          atomic.Pointer[service.SiteModelRouter]
}

func NewSiteStore(db *sql.DB, cfg *config.Config) (*SiteStore, error) {
	s := &SiteStore{db: db, cfg: cfg.Site, tables: siteTables()}
	s.ExportRequestPolicy, s.InstallRequestPolicy = cfg.ExportSiteRequestPolicy, cfg.InstallSiteRequestPolicy
	if cfg.Site.EffectiveMode() == site.Standalone {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.bindIdentity(ctx); err != nil {
		return nil, err
	}
	if cfg.IsSecondary() {
		for _, trigger := range []struct{ name, table, event string }{
			{"site_capture_billing", "usage_billing_events", "UPDATE OF state"},
			{"site_capture_error", "ops_error_logs", "INSERT OR UPDATE"},
			{"site_capture_trace", "request_connection_traces", "INSERT OR UPDATE"},
		} {
			var exists bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname=$1 AND tgrelid=$2::regclass)`, trigger.name, trigger.table).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				if _, err := db.ExecContext(ctx, `CREATE TRIGGER `+trigger.name+` AFTER `+trigger.event+` ON `+trigger.table+` FOR EACH ROW EXECUTE FUNCTION site_capture_event()`); err != nil {
					return nil, err
				}
			}
		}
	}
	h := sha256.New()
	for i := range s.tables {
		t := &s.tables[i]
		rows, err := db.QueryContext(ctx, `SELECT column_name,concat_ws(':',udt_name,character_maximum_length,numeric_precision,numeric_scale,is_nullable) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`, t.name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, kind string
			if err = rows.Scan(&name, &kind); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if t.name != "settings" || name != "id" {
				t.columns = append(t.columns, name)
			}
			fmt.Fprintf(h, "%s:%s:%s;", t.name, name, kind)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if len(t.columns) == 0 {
			return nil, fmt.Errorf("site schema missing table %s", t.name)
		}
	}
	s.schema = hex.EncodeToString(h.Sum(nil))
	return s, nil
}

func (s *SiteStore) bindIdentity(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, siteFence); err != nil {
		return err
	}
	var mode, id string
	err = tx.QueryRowContext(ctx, `SELECT mode,node_id FROM site_identity WHERE singleton`).Scan(&mode, &id)
	if err == nil {
		if mode != s.cfg.EffectiveMode() || id != s.cfg.ID {
			return errors.New("site database identity mismatch; use an independent database for each site")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if s.cfg.EffectiveMode() == site.Secondary {
		var used bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys) OR EXISTS(SELECT 1 FROM accounts) OR EXISTS(SELECT 1 FROM usage_billing_events) OR (SELECT count(*)>1 FROM users) OR EXISTS(SELECT 1 FROM users WHERE role<>'admin')`).Scan(&used); err != nil {
			return err
		}
		if used {
			return errors.New("secondary requires a fresh dedicated database; refusing to overwrite existing keys, accounts or billing")
		}
		// Auto-setup may have created a local administrator. There is no panel on
		// a secondary; remove only this otherwise unused bootstrap identity.
		if _, err = tx.ExecContext(ctx, `DELETE FROM users`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO site_identity(singleton,mode,node_id) VALUES(true,$1,$2)`, s.cfg.EffectiveMode(), s.cfg.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SiteStore) Schema() string { return s.schema }

func (s *SiteStore) Pending(ctx context.Context, limit int) ([]site.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,payload FROM site_outbox ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []site.Event
	for rows.Next() {
		var e site.Event
		if err = rows.Scan(&e.ID, &e.Kind, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SiteStore) PendingIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM site_outbox ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SiteStore) Export(ctx context.Context, p site.Peer, events []site.Event, pendingIDs []int64) (site.Snapshot, error) {
	// A primary request or another peer may update the same user during our
	// repeatable-read transaction. Retry the entire snapshot/receipt transaction;
	// never weaken its consistency to make that concurrent update disappear.
	for attempt := 0; ; attempt++ {
		result, err := s.exportOnce(ctx, p, events, pendingIDs)
		var pgErr *pq.Error
		if err == nil || attempt >= 5 || !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return result, err
		}
		timer := time.NewTimer((10 * time.Millisecond) << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return site.Snapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *SiteStore) exportOnce(ctx context.Context, p site.Peer, events []site.Event, pendingIDs []int64) (site.Snapshot, error) {
	var result site.Snapshot
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	var imported []service.UsageBillingEvent
	accountRows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE `+siteAccountGrantSQL+` ORDER BY id`)
	if err != nil {
		return result, err
	}
	var accountIDs []int64
	for accountRows.Next() {
		var id int64
		if err = accountRows.Scan(&id); err != nil {
			_ = accountRows.Close()
			return result, err
		}
		accountIDs = append(accountIDs, id)
	}
	err = accountRows.Err()
	_ = accountRows.Close()
	if err != nil {
		return result, err
	}
	for _, id := range accountIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_account_grants(node_id,account_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.ID, id); err != nil {
			return result, err
		}
	}

	for _, e := range events {
		bill, err := s.importEvent(ctx, tx, p, e)
		if err != nil {
			return result, err
		}
		if bill != nil {
			imported = append(imported, *bill)
		}
	}
	// Receipt lookup and balances share this transaction. This also acknowledges
	// previously accepted events outside the current page (e.g. a late-committing
	// lower outbox ID). Never infer acknowledgement from a sequence high-water.
	ackRows, err := tx.QueryContext(ctx, `SELECT event_id FROM site_receipts WHERE node_id=$1 AND event_id=ANY($2::bigint[]) ORDER BY event_id`, p.ID, pq.Array(pendingIDs))
	if err != nil {
		return result, err
	}
	for ackRows.Next() {
		var id int64
		if err = ackRows.Scan(&id); err != nil {
			_ = ackRows.Close()
			return result, err
		}
		result.Acknowledged = append(result.Acknowledged, id)
	}
	err = ackRows.Err()
	_ = ackRows.Close()
	if err != nil {
		return result, err
	}
	ids := make([]string, len(accountIDs))
	for i, id := range accountIDs {
		ids[i] = strconv.FormatInt(id, 10)
	}
	if len(ids) == 0 {
		ids = []string{"NULL"}
	}
	result.Schema = s.schema
	for _, t := range s.tables {
		where := ""
		if t.filter != "" {
			where = " WHERE " + fmt.Sprintf(t.filter, strings.Join(ids, ","))
		}
		if t.name == "settings" {
			where = " WHERE " + siteSettingsPredicate()
		}
		projection := "to_jsonb(t)"
		if t.name == "accounts" {
			// Pause authorities live outside the account row. Project their
			// effective decision without replicating the controllers/jobs.
			projection += ` || jsonb_build_object('schedulable',t.schedulable AND NOT EXISTS(SELECT 1 FROM account_performance_monitors m WHERE m.account_id=t.id AND m.paused) AND NOT EXISTS(SELECT 1 FROM ai_ops_controls a WHERE a.account_id=t.id AND a.ai_paused))`
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+projection+` FROM "`+t.name+`" t`+where)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return result, err
			}
			value, err := redactSiteRow(t.name, raw, p)
			if err != nil {
				_ = rows.Close()
				return result, err
			}
			if t.name == "settings" {
				value, err = s.projectSiteSetting(value, false)
				if err != nil {
					_ = rows.Close()
					return result, err
				}
			}
			key, err := siteRowKey(t, value)
			if err != nil {
				_ = rows.Close()
				return result, err
			}
			result.Rows = append(result.Rows, site.Row{Table: t.name, Key: key, Value: value})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return result, err
		}
	}
	quality, err := exportSiteQuality(ctx, tx, accountIDs)
	if err != nil {
		return result, err
	}
	result.Rows = append(result.Rows, quality...)
	if err = tx.Commit(); err != nil {
		return result, err
	}
	if s.ExportPricing != nil {
		raw, e := s.ExportPricing()
		if e != nil {
			return result, e
		}
		result.Rows = append(result.Rows, site.Row{Table: "$pricing", Key: "catalog", Value: raw})
	}
	if s.ExportRequestPolicy != nil {
		raw, e := s.ExportRequestPolicy()
		if e != nil {
			return result, e
		}
		result.Rows = append(result.Rows, site.Row{Table: "$policy", Key: "request", Value: raw})
	}
	if s.AfterImport != nil {
		if err = s.AfterImport(ctx, imported); err != nil {
			return result, err
		}
	}
	return result, nil
}

func redactSiteRow(table string, raw []byte, p site.Peer) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	set := func(k string, v any) {
		if _, ok := obj[k]; ok {
			obj[k], _ = json.Marshal(v)
		}
	}
	switch table {
	case "users":
		set("password_hash", "!site-replica-no-login")
		set("email", "site-user-"+string(obj["id"])+"@replica.invalid")
		set("totp_secret_encrypted", nil)
		set("totp_enabled", false)
		set("totp_enabled_at", nil)
		set("username", "")
		set("notes", "")
		set("balance_notify_extra_emails", "[]")
		set("balance_notify_enabled", false)
	case "api_keys":
		var key string
		if err := json.Unmarshal(obj["key"], &key); err != nil {
			return nil, err
		}
		set("key", site.KeyDigest(p.Token, key))
	case "accounts":
		var id int64
		_ = json.Unmarshal(obj["id"], &id)
		var kind string
		_ = json.Unmarshal(obj["type"], &kind)
		if kind != service.AccountTypeAPIKey {
			return nil, errors.New("site account must be an API-key account")
		}
		if ref := obj["parent_account_id"]; len(ref) > 0 && string(ref) != "null" {
			return nil, errors.New("site accounts must own their API-key credentials")
		}
		set("proxy_id", nil) // domestic requests must not inherit a US SOCKS proxy
		set("proxy_fallback_origin_id", nil)
		if base := p.BaseURLs[id]; base != "" {
			var creds map[string]json.RawMessage
			if err := json.Unmarshal(obj["credentials"], &creds); err != nil {
				return nil, err
			}
			creds["base_url"], _ = json.Marshal(base)
			// Explicit per-protocol relay URLs take precedence over base_url in
			// the existing gateway. Drop them when selecting a domestic origin,
			// otherwise a US relay silently survives this override.
			delete(creds, "api_base_urls")
			obj["credentials"], _ = json.Marshal(creds)
		}
	case "user_subscriptions":
		set("assigned_by", nil)
		set("notes", "")
	case "settings":
		var key, rawValue string
		_ = json.Unmarshal(obj["key"], &key)
		if key == service.SettingKeyOAuthAutoConfig {
			_ = json.Unmarshal(obj["value"], &rawValue)
			var value map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rawValue), &value); err != nil {
				return nil, err
			}
			projection := map[string]json.RawMessage{}
			if v, ok := value["model_billing"]; ok {
				projection["model_billing"] = v
			}
			encoded, _ := json.Marshal(projection)
			set("value", string(encoded))
		}
	}
	return json.Marshal(obj)
}

func siteRowKey(t siteTable, raw []byte) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	if t.name == "settings" {
		var key string
		if err := json.Unmarshal(obj["key"], &key); err != nil || key == "" {
			return "", errors.New("invalid setting key")
		}
		return key, nil
	}
	parts := make([]string, len(t.keys))
	for i, k := range t.keys {
		v, err := strconv.ParseInt(string(obj[k]), 10, 64)
		if err != nil || v <= 0 {
			return "", fmt.Errorf("invalid %s primary key", t.name)
		}
		parts[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(parts, ","), nil
}

func (s *SiteStore) Install(ctx context.Context, response site.SyncResponse) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, siteFence); err != nil {
		return err
	}
	var primary, version string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(primary_id,''),version FROM site_identity WHERE singleton FOR UPDATE`).Scan(&primary, &version); err != nil {
		return err
	}
	if primary != "" && primary != response.PrimaryID {
		return errors.New("replica belongs to another primary")
	}
	if !response.Full && version != response.BaseVersion {
		return errors.New("replica diff base mismatch")
	}
	allowed := map[string]siteTable{}
	dirty := map[string]bool{}
	changed := map[string]bool{"$full": response.Full}
	for _, t := range s.tables {
		allowed[t.name] = t
		if response.Full {
			dirty[t.name] = true
		}
	}
	// Keep the previous primary account projection to distinguish policy changes
	// from heartbeat/balance refreshes. Local cooldowns must survive the latter.
	var previousAccounts []byte
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_object_agg(row_key,value),'{}'::jsonb) FROM site_replica_rows WHERE table_name='accounts'`).Scan(&previousAccounts); err != nil {
		return err
	}
	var hadPendingBills bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM site_outbox WHERE kind='billing')`).Scan(&hadPendingBills); err != nil {
		return err
	}
	if hadPendingBills {
		// Reset these baselines before replay, including bills acknowledged in
		// this response. Unchanged rows are excluded from UPDATE below.
		for _, name := range []string{"users", "api_keys", "user_subscriptions", "accounts", "usage_platform_quota_totals"} {
			dirty[name] = true
		}
	}
	if response.Full {
		if _, err = tx.ExecContext(ctx, `DELETE FROM site_replica_rows`); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, r := range response.Rows {
		changed[r.Table] = true
		if r.Table == "$quality" {
			if seen[r.Identity()] || len(r.Key) != 64 {
				return errors.New("invalid quality projection")
			}
			seen[r.Identity()] = true
			if len(r.Value) == 0 {
				_, err = tx.ExecContext(ctx, `DELETE FROM site_replica_rows WHERE table_name='$quality' AND row_key=$1`, r.Key)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO site_replica_rows(table_name,row_key,value) VALUES('$quality',$1,$2::jsonb) ON CONFLICT(table_name,row_key) DO UPDATE SET value=EXCLUDED.value`, r.Key, string(r.Value))
			}
			if err != nil {
				return err
			}
			continue
		}
		if ((r.Table == "$pricing" && r.Key == "catalog") || (r.Table == "$policy" && r.Key == "request")) && len(r.Value) > 0 {
			if seen[r.Identity()] {
				return errors.New("duplicate pricing catalog")
			}
			seen[r.Identity()] = true
			_, err = tx.ExecContext(ctx, `INSERT INTO site_replica_rows(table_name,row_key,value) VALUES($1,$2,$3::jsonb) ON CONFLICT(table_name,row_key) DO UPDATE SET value=EXCLUDED.value`, r.Table, r.Key, string(r.Value))
			if err != nil {
				return err
			}
			continue
		}
		t, ok := allowed[r.Table]
		if !ok || seen[r.Identity()] {
			return errors.New("unknown or duplicate replica row")
		}
		if r.Table == "settings" && !service.IsSiteSetting(r.Key) {
			return errors.New("setting outside site request policy")
		}
		seen[r.Identity()] = true
		dirty[r.Table] = true
		if len(r.Value) == 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM site_replica_rows WHERE table_name=$1 AND row_key=$2`, r.Table, r.Key)
		} else {
			key, e := siteRowKey(t, r.Value)
			if e != nil || key != r.Key {
				return errors.New("invalid replica row key")
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO site_replica_rows(table_name,row_key,value) VALUES($1,$2,$3::jsonb) ON CONFLICT(table_name,row_key) DO UPDATE SET value=EXCLUDED.value`, r.Table, r.Key, string(r.Value))
		}
		if err != nil {
			return err
		}
	}
	for _, id := range response.Acknowledged {
		if _, err = tx.ExecContext(ctx, `DELETE FROM site_outbox WHERE id=$1`, id); err != nil {
			return err
		}
	}
	// Policies are replaced from the immutable primary baseline. Monetary
	// tables are then overlaid with precisely the still-unacknowledged debits.
	if err = s.removeAbsentRows(ctx, tx, dirty); err != nil {
		return err
	}
	for _, t := range s.tables {
		if !dirty[t.name] {
			continue
		}
		if err = s.installTable(ctx, tx, t, previousAccounts); err != nil {
			return fmt.Errorf("install %s: %w", t.name, err)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM site_outbox WHERE kind='billing' ORDER BY id`)
	if err != nil {
		return err
	}
	var pending []service.UsageBillingEvent
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		var e service.UsageBillingEvent
		if err = json.Unmarshal(raw, &e); err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	baselineIDs := map[string]bool{}
	if len(pending) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT table_name||':'||row_key FROM site_replica_rows WHERE table_name IN ('users','api_keys','accounts','user_subscriptions')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			baselineIDs[id] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	for _, e := range pending {
		// Removed targets retain their last local state for historical FKs. They
		// were not reset from a new baseline, so must not receive the overlay a
		// second time. The unmodified outbox event still settles on the primary.
		c := &e.Command
		if !baselineIDs["users:"+strconv.FormatInt(c.UserID, 10)] {
			c.BalanceCost = 0
		}
		if !baselineIDs["api_keys:"+strconv.FormatInt(c.APIKeyID, 10)] {
			c.APIKeyQuotaCost, c.APIKeyRateLimitCost = 0, 0
		}
		if !baselineIDs["accounts:"+strconv.FormatInt(c.AccountID, 10)] {
			c.AccountQuotaCost = 0
		}
		if c.SubscriptionID != nil && !baselineIDs["user_subscriptions:"+strconv.FormatInt(*c.SubscriptionID, 10)] {
			c.SubscriptionCost = 0
		}
		if err = replaySiteDebit(ctx, tx, &e); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE site_identity SET version=$1,primary_id=$2 WHERE singleton`, response.Version, response.PrimaryID)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// A heartbeat only renews the lease. Do not rebuild clients/caches
	// when the authoritative snapshot and pending overlays are unchanged.
	if !response.Full && len(response.Rows) == 0 && !hadPendingBills && len(response.Acknowledged) == 0 {
		return nil
	}
	if s.InstallPricing != nil && (response.Full || changed["$pricing"]) {
		var raw []byte
		if err = s.db.QueryRowContext(ctx, `SELECT value FROM site_replica_rows WHERE table_name='$pricing' AND row_key='catalog'`).Scan(&raw); err != nil {
			return err
		}
		if err = s.InstallPricing(raw); err != nil {
			return err
		}
	}
	if s.InstallRequestPolicy != nil && (response.Full || changed["$policy"]) {
		var raw []byte
		if err = s.db.QueryRowContext(ctx, `SELECT value FROM site_replica_rows WHERE table_name='$policy' AND row_key='request'`).Scan(&raw); err != nil {
			return err
		}
		if err = s.InstallRequestPolicy(raw); err != nil {
			return err
		}
	}
	if s.AfterInstall != nil {
		if err := s.AfterInstall(ctx); err != nil {
			return err
		}
	}
	if s.AfterPolicyInstall != nil {
		if err := s.AfterPolicyInstall(ctx, changed); err != nil {
			return err
		}
	}
	return s.refreshModelRouter(ctx)
}

func quotedColumns(columns []string) string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = `"` + c + `"`
	}
	return strings.Join(out, ",")
}

func (s *SiteStore) installTable(ctx context.Context, tx *sql.Tx, t siteTable, previousAccounts []byte) error {
	if t.name == "settings" {
		return s.installSiteSettings(ctx, tx)
	}
	columns := quotedColumns(t.columns)
	var updates, before, after []string
	for _, c := range t.columns {
		isKey := false
		for _, k := range t.keys {
			if c == k {
				isKey = true
			}
		}
		// Updating status fires a revision trigger even when the value did not
		// change. Apply primary status/revision separately from monetary updates.
		if t.name == "users" && (c == "status" || c == "moderation_status_revision") {
			continue
		}
		if !isKey {
			value := `EXCLUDED."` + c + `"`
			if t.name == "accounts" {
				value = siteAccountUpdateValue(c)
			}
			updates = append(updates, `"`+c+`"=`+value)
			before = append(before, `"`+t.name+`"."`+c+`"`)
			after = append(after, value)
		}
	}
	conflict := "DO NOTHING"
	if len(updates) > 0 {
		conflict = "DO UPDATE SET " + strings.Join(updates, ",") + " WHERE ROW(" + strings.Join(before, ",") + ") IS DISTINCT FROM ROW(" + strings.Join(after, ",") + ")"
	}
	query := `INSERT INTO "` + t.name + `" (` + columns + `) SELECT ` + columns + ` FROM jsonb_populate_recordset(NULL::"` + t.name + `",COALESCE((SELECT jsonb_agg(value) FROM site_replica_rows WHERE table_name=$1),'[]'::jsonb)) ON CONFLICT (` + quotedColumns(t.keys) + `) ` + conflict
	args := []any{t.name}
	if t.name == "accounts" {
		args = append(args, string(previousAccounts))
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	if t.name == "users" {
		if _, err := tx.ExecContext(ctx, `UPDATE users u SET status=r.value->>'status' FROM site_replica_rows r WHERE r.table_name='users' AND r.row_key=u.id::text AND u.status IS DISTINCT FROM r.value->>'status'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE users u SET moderation_status_revision=(r.value->>'moderation_status_revision')::bigint FROM site_replica_rows r WHERE r.table_name='users' AND r.row_key=u.id::text AND u.moderation_status_revision IS DISTINCT FROM (r.value->>'moderation_status_revision')::bigint`)
		return err
	}
	return nil
}

func siteAccountUpdateValue(column string) string {
	value := `EXCLUDED."` + column + `"`
	old := `($2::jsonb->accounts.id::text)`
	unchangedPolicy := old + `->'credentials' IS NOT DISTINCT FROM to_jsonb(EXCLUDED.credentials) AND ` + old + `->'status' IS NOT DISTINCT FROM to_jsonb(EXCLUDED.status) AND ` + old + `->'schedulable' IS NOT DISTINCT FROM to_jsonb(EXCLUDED.schedulable)`
	switch column {
	case "status", "schedulable", "error_message", "rate_limited_at", "rate_limit_reset_at", "overload_until", "temp_unschedulable_until", "temp_unschedulable_reason", "session_window_start", "session_window_end", "session_window_status":
		return `CASE WHEN ` + unchangedPolicy + ` AND ` + old + `->'` + column + `' IS NOT DISTINCT FROM to_jsonb(EXCLUDED)->'` + column + `' THEN accounts."` + column + `" ELSE ` + value + ` END`
	case "last_used_at":
		return `GREATEST(accounts.last_used_at,EXCLUDED.last_used_at)`
	case "extra":
		// Keep local per-model rate limits while restoring only monetary counters
		// and authoritative settings from the primary baseline.
		return `CASE WHEN ` + unchangedPolicy + ` AND ` + old + `->'extra'->'model_rate_limits' IS NOT DISTINCT FROM EXCLUDED.extra->'model_rate_limits' THEN (COALESCE(EXCLUDED.extra,'{}'::jsonb)-'model_rate_limits') || CASE WHEN accounts.extra ? 'model_rate_limits' THEN jsonb_build_object('model_rate_limits',accounts.extra->'model_rate_limits') ELSE '{}'::jsonb END ELSE ` + value + ` END`
	}
	return value
}

func rowKeySQL(t siteTable, alias string) string {
	parts := make([]string, len(t.keys))
	for i, k := range t.keys {
		parts[i] = alias + `."` + k + `"::text`
	}
	return strings.Join(parts, `||','||`)
}

func (s *SiteStore) removeAbsentRows(ctx context.Context, tx *sql.Tx, dirty map[string]bool) error {
	for i := len(s.tables) - 1; i >= 0; i-- {
		t := s.tables[i]
		if !dirty[t.name] {
			continue
		}
		condition := `NOT EXISTS(SELECT 1 FROM site_replica_rows r WHERE r.table_name=$1 AND r.row_key=` + rowKeySQL(t, "t") + `)`
		query := `DELETE FROM "` + t.name + `" t WHERE ` + condition
		switch t.name {
		case "users", "api_keys", "groups", "accounts", "user_subscriptions", "user_platform_quotas":
			// Keep FK targets for in-flight requests/historical local logs, but
			// immediately make deleted/revoked data ineligible for new requests.
			// Release live partial-unique keys before inserting a replacement. A
			// soft-deleted primary row can still be present in the snapshot.
			query = `UPDATE "` + t.name + `" t SET deleted_at=NOW() WHERE deleted_at IS NULL AND (` + condition + ` OR EXISTS(SELECT 1 FROM site_replica_rows r WHERE r.table_name=$1 AND r.row_key=` + rowKeySQL(t, "t") + ` AND r.value->>'deleted_at' IS NOT NULL))`
		case "settings":
			query += " AND " + siteSettingsPredicate()
		}
		if _, err := tx.ExecContext(ctx, query, t.name); err != nil {
			return err
		}
	}
	return nil
}

// Build from the committed primary baseline, not local cooldown/usage overlays.
// Thus one transient upstream failure does not change the request's location.
func (s *SiteStore) refreshModelRouter(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT table_name,value FROM site_replica_rows WHERE table_name IN ('accounts','account_groups','composite_model_routes','groups','api_keys','api_key_groups') ORDER BY table_name,row_key`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var accounts []service.Account
	var routes []service.CompositeModelRoute
	var groups []service.Group
	var keys []service.APIKey
	bindings := map[int64][]service.AccountGroup{}
	keyGroups := map[int64][]int64{}
	for rows.Next() {
		var table string
		var raw []byte
		if err := rows.Scan(&table, &raw); err != nil {
			return err
		}
		switch table {
		case "accounts":
			var a struct {
				ID                     int64
				Platform, Type, Status string
				Schedulable            bool
				Credentials, Extra     map[string]any
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			accounts = append(accounts, service.Account{ID: a.ID, Platform: a.Platform, Type: a.Type, Status: a.Status, Schedulable: a.Schedulable, Credentials: a.Credentials, Extra: a.Extra})
		case "account_groups":
			var a struct {
				AccountID     int64    `json:"account_id"`
				GroupID       int64    `json:"group_id"`
				AllowedModels []string `json:"allowed_models"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			bindings[a.AccountID] = append(bindings[a.AccountID], service.AccountGroup{AccountID: a.AccountID, GroupID: a.GroupID, AllowedModels: a.AllowedModels})
		case "composite_model_routes":
			var route service.CompositeModelRoute
			if err := json.Unmarshal(raw, &route); err != nil {
				return err
			}
			routes = append(routes, route)
		case "groups":
			var g struct {
				ID               int64
				Platform, Status string
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				return err
			}
			groups = append(groups, service.Group{ID: g.ID, Platform: g.Platform, Status: g.Status})
		case "api_keys":
			var k struct {
				ID           int64
				Key          string
				GroupID      *int64            `json:"group_id"`
				ModelMapping map[string]string `json:"model_mapping"`
			}
			if err := json.Unmarshal(raw, &k); err != nil {
				return err
			}
			keys = append(keys, service.APIKey{ID: k.ID, Key: k.Key, GroupID: k.GroupID, ModelMapping: k.ModelMapping})
		case "api_key_groups":
			var g struct {
				KeyID   int64 `json:"api_key_id"`
				GroupID int64 `json:"group_id"`
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				return err
			}
			keyGroups[g.KeyID] = append(keyGroups[g.KeyID], g.GroupID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range accounts {
		accounts[i].AccountGroups = bindings[accounts[i].ID]
		for _, binding := range accounts[i].AccountGroups {
			accounts[i].GroupIDs = append(accounts[i].GroupIDs, binding.GroupID)
		}
	}
	for i := range keys {
		keys[i].GroupIDs = keyGroups[keys[i].ID]
	}
	s.modelRouter.Store(service.NewSiteModelRouter(accounts, routes, groups, keys, s.MapChannelModel))
	return nil
}

func (s *SiteStore) MatchLocalModel(key, model, endpoint string) bool {
	return s.modelRouter.Load().Match(site.KeyDigest(s.cfg.Token, key), model, endpoint)
}
