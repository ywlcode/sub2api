package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *SiteStore) initializeProjection(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, siteFence); err != nil {
		return err
	}
	tables := append([]siteTable(nil), s.tables...)
	for _, name := range []string{"account_performance_monitors", "ai_ops_controls", "scheduled_test_plans", "scheduled_test_results"} {
		tables = append(tables, siteTable{name: name})
	}
	for _, t := range tables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='site_policy_change' AND tgrelid=$1::regclass)`, t.name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `CREATE TRIGGER site_policy_change AFTER INSERT OR UPDATE OR DELETE ON `+pq.QuoteIdentifier(t.name)+` FOR EACH ROW EXECUTE FUNCTION site_capture_change()`); err != nil {
				return err
			}
		}
	}
	// Moderation stays on the primary. Only eligibility changes enqueue the
	// existing user/key rows that the secondary uses for admission.
	var installed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='site_moderation_change' AND tgrelid='content_moderation_user_state'::regclass)`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		if _, err = tx.ExecContext(ctx, `CREATE TRIGGER site_moderation_change AFTER INSERT OR UPDATE OR DELETE ON content_moderation_user_state FOR EACH ROW EXECUTE FUNCTION site_capture_moderation_change()`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *SiteStore) registerProjectionOn(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, p site.Peer) error {
	raw, _ := json.Marshal(struct {
		Schema, Revision, Token string
		Bases                   map[int64]string
		Fields                  map[string][]siteColumn
		Settings                []string
	}{s.projectionSchema(p), siteProjectionRevision, p.Token, p.BaseURLs, siteContractColumns, service.SiteSettingKeys})
	sum := sha256.Sum256(raw)
	_, err := executor.ExecContext(ctx, `INSERT INTO site_export_nodes(node_id,fingerprint,epoch) SELECT $1,$2,$3 WHERE NOT EXISTS(SELECT 1 FROM site_export_nodes WHERE node_id=$1 AND fingerprint=$2) ON CONFLICT(node_id) DO UPDATE SET fingerprint=EXCLUDED.fingerprint,epoch=EXCLUDED.epoch,initialized=false,retained_revision=0 WHERE site_export_nodes.fingerprint<>EXCLUDED.fingerprint`, p.ID, hex.EncodeToString(sum[:]), uuid.NewString())
	return err
}

func (s *SiteStore) ExportPage(ctx context.Context, p site.Peer, input site.SyncRequest) (site.Snapshot, error) {
	p.ReplicaID, p.ConfigSchema = input.ReplicaID, input.Schema
	return s.exportRetry(ctx, p, input, nil, true)
}
func (s *SiteStore) Export(ctx context.Context, p site.Peer, events []site.Event, ids []int64) (site.Snapshot, error) {
	return s.exportRetry(ctx, p, site.SyncRequest{PendingIDs: ids}, events, false)
}

// Serialize exporters before starting the snapshot. This lock is private to
// policy publication; billing never takes it and never waits for the snapshot.
func (s *SiteStore) projectionConnection(ctx context.Context, node string) (*sql.Conn, func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, e := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended('site-projection:'||$1,0))`, node); e != nil {
			// Never return a session holding a lock to the pool, including cancellation
			// during lock acquisition when the server may already have granted it.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended('site-projection:'||$1,0))`, node); err != nil {
		release()
		return nil, nil, err
	}
	return conn, release, nil
}

func (s *SiteStore) exportRetry(ctx context.Context, p site.Peer, input site.SyncRequest, events []site.Event, paged bool) (site.Snapshot, error) {
	p.ID, p.ReplicaID = p.StorageID(), ""
	conn, release, err := s.projectionConnection(ctx, p.ID)
	if err != nil {
		return site.Snapshot{}, err
	}
	defer release()
	if err := s.registerProjectionOn(ctx, conn, p); err != nil {
		return site.Snapshot{}, err
	}
	if err := s.confirmSiteSettlements(ctx, conn, p.ID, input.Version); err != nil {
		return site.Snapshot{}, err
	}
	// Legacy in-process callers also import outside the export snapshot. Runtime
	// delivery already uses its independent transaction and receipt lock.
	if len(events) > 0 {
		if _, err := s.Deliver(ctx, p, events); err != nil {
			return site.Snapshot{}, err
		}
		known := map[int64]bool{}
		for _, id := range input.PendingIDs {
			known[id] = true
		}
		for _, e := range events {
			if !known[e.ID] {
				input.PendingIDs = append(input.PendingIDs, e.ID)
				known[e.ID] = true
			}
		}
	}
	return s.exportProjection(ctx, conn, p, input, paged)
}

func (s *SiteStore) exportProjection(ctx context.Context, conn *sql.Conn, p site.Peer, input site.SyncRequest, paged bool) (site.Snapshot, error) {
	out := site.Snapshot{Schema: s.projectionSchema(p), ReceiptDigests: map[int64]string{}}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	var revision, retained int64
	var epoch string
	var initialized bool
	if err = tx.QueryRowContext(ctx, `SELECT epoch,revision,initialized,retained_revision FROM site_export_nodes WHERE node_id=$1`, p.ID).Scan(&epoch, &revision, &initialized, &retained); err != nil {
		return out, err
	}
	current := epoch + ":" + strconv.FormatInt(revision, 10)
	if input.Cursor != "" && input.TargetVersion != current {
		return out, errors.New("site snapshot changed; restart paging")
	}
	var observed []siteChangeGeneration
	if input.Cursor == "" {
		if !initialized {
			if _, err = tx.ExecContext(ctx, `DELETE FROM site_export_rows WHERE node_id=$1`, p.ID); err != nil {
				return out, err
			}
		}
		// Grants survive revocation for requests already in flight.
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_account_grants(node_id,account_id) SELECT $1,id FROM accounts WHERE `+siteAccountGrantSQL+` ON CONFLICT DO NOTHING`, p.ID); err != nil {
			return out, err
		}
		var e error
		observed, e = readSiteChanges(ctx, tx, p.ID)
		if e != nil {
			return out, e
		}
		changed, e := s.refreshProjection(ctx, tx, p, revision+1, !initialized, observed)
		if e != nil {
			return out, e
		}
		if changed || !initialized {
			revision++
			if _, err = tx.ExecContext(ctx, `UPDATE site_export_nodes SET revision=$2,initialized=true WHERE node_id=$1`, p.ID, revision); err != nil {
				return out, err
			}
		}
	}
	out.Version = epoch + ":" + strconv.FormatInt(revision, 10)
	base := int64(0)
	if strings.HasPrefix(input.Version, epoch+":") {
		base, _ = strconv.ParseInt(strings.TrimPrefix(input.Version, epoch+":"), 10, 64)
	}
	if input.Cursor != "" && base > 0 && base < retained {
		return out, errors.New("site delta history expired; restart paging")
	}
	out.Full = base <= 0 || base > revision || base < retained
	if out.Full {
		base = 0
	}
	// Receipts and monetary rows share the repeatable-read snapshot, including
	// accepted uploads whose HTTP response was lost.
	ack, err := tx.QueryContext(ctx, `SELECT r.event_id,r.digest,CASE WHEN q.state IN ('settled','waived') THEN '' ELSE r.rejection END FROM site_receipts r LEFT JOIN site_rejected_events q USING(node_id,event_id) WHERE r.node_id=$1 AND r.event_id=ANY($2::bigint[]) ORDER BY r.event_id`, p.ID, pq.Array(input.PendingIDs))
	if err != nil {
		return out, err
	}
	for ack.Next() {
		var id int64
		var reason, digest string
		if err = ack.Scan(&id, &digest, &reason); err != nil {
			_ = ack.Close()
			return out, err
		}
		if input.PendingDigests != nil && input.PendingDigests[id] != digest {
			continue
		}
		out.ReceiptDigests[id] = digest
		if reason == "" {
			out.Acknowledged = append(out.Acknowledged, id)
		} else {
			out.Rejected = append(out.Rejected, site.Rejection{ID: id, Reason: reason})
		}
	}
	err = ack.Err()
	_ = ack.Close()
	if err != nil {
		return out, err
	}
	limit := int64(1 << 60)
	if paged {
		limit = 1025
	}
	rows, err := tx.QueryContext(ctx, `SELECT table_name,row_key,value FROM site_export_rows WHERE node_id=$1 AND revision>$2 AND (NOT $3 OR value IS NOT NULL) AND (table_name||':'||row_key) COLLATE "C">$4 COLLATE "C" ORDER BY (table_name||':'||row_key) COLLATE "C" LIMIT $5`, p.ID, base, out.Full, input.Cursor, limit)
	if err != nil {
		return out, err
	}
	size := 0
	for rows.Next() {
		var row site.Row
		var raw []byte
		if err = rows.Scan(&row.Table, &row.Key, &raw); err != nil {
			_ = rows.Close()
			return out, err
		}
		row.Value = raw
		if paged && len(out.Rows) > 0 && (size+len(row.Value) > 2<<20 || len(out.Rows) >= 1024) {
			out.Cursor = out.Rows[len(out.Rows)-1].Identity()
			break
		}
		if len(row.Value) > 30<<20 {
			_ = rows.Close()
			return out, errors.New("single site policy row exceeds 30 MiB")
		}
		size += len(row.Value)
		out.Rows = append(out.Rows, row)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	// Only publication has committed. A failed cleanup repeats work next time;
	// it never invalidates the published balance/receipt snapshot.
	if err = acknowledgeSiteChanges(ctx, conn, p.ID, observed); err != nil {
		slog.Warn("site change cleanup deferred; published snapshot remains valid", "node", p.ID, "error", err)
	}
	return out, nil
}

const siteScopeGroups = `SELECT group_id FROM account_groups WHERE account_id IN (SELECT id FROM accounts WHERE ` + siteAccountGrantSQL + `)`

func siteScopedKeys() string {
	return `SELECT k.id FROM api_keys k WHERE k.deleted_at IS NULL AND (k.group_id IN (` + siteScopeGroups + `) OR EXISTS(SELECT 1 FROM api_key_groups kg WHERE kg.api_key_id=k.id AND kg.group_id IN (` + siteScopeGroups + `)))`
}
func siteScopedUsers() string {
	return `SELECT user_id FROM api_keys WHERE id IN (` + siteScopedKeys() + `)`
}

// Discovery publishes public metadata for all groups a replicated key can
// select, including bindings without a local upstream account. Neither the key
// itself nor any ungranted account credential is selected by this query.
func (s *SiteStore) DiscoveryGroupIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `WITH scoped AS (
 SELECT id,group_id FROM api_keys WHERE id IN (`+siteScopedKeys()+`)
), candidates AS (
 SELECT group_id FROM scoped UNION
 SELECT kg.group_id FROM api_key_groups kg JOIN scoped ON scoped.id=kg.api_key_id
)
 SELECT g.id FROM groups g JOIN candidates c ON c.group_id=g.id
 WHERE g.deleted_at IS NULL AND g.status='active' ORDER BY g.id LIMIT 256`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SiteStore) siteFilter(t siteTable) string {
	switch t.name {
	case "users":
		return `t.id IN (` + siteScopedUsers() + `)`
	case "api_keys":
		return `t.id IN (` + siteScopedKeys() + `)`
	case "api_key_groups":
		return `t.api_key_id IN (` + siteScopedKeys() + `)`
	case "user_allowed_groups", "user_group_rate_multipliers", "user_subscriptions", "user_platform_quotas":
		return `t.user_id IN (` + siteScopedUsers() + `)`
	case "usage_platform_quota_totals":
		return `t.quota_id IN (SELECT id FROM user_platform_quotas WHERE user_id IN (` + siteScopedUsers() + `))`
	case "settings":
		return siteSettingsPredicate()
	}
	if t.filter != "" {
		return "(" + fmt.Sprintf(t.filter, `SELECT id FROM accounts WHERE `+siteAccountGrantSQL) + ")"
	}
	return "true"
}

func (s *SiteStore) refreshProjection(ctx context.Context, tx *sql.Tx, p site.Peer, revision int64, full bool, observed []siteChangeGeneration) (bool, error) {
	dirty := map[string]map[string]bool{}
	for _, change := range observed {
		if dirty[change.Table] == nil {
			dirty[change.Table] = map[string]bool{}
		}
		dirty[change.Table][change.Key] = true
	}
	var err error
	var rows *sql.Rows
	full = full || dirty["$scope"] != nil
	legacy := siteLegacySchemas[s.projectionSchema(p)]
	var legacyPricingUnsupported bool
	if legacy {
		// Old replicas cannot apply 12-hour cache-write prices. While mixed
		// versions run, keep OpenAI billing on the primary if those prices are enabled.
		// The four small configuration tables are independent of usage volume.
		var predicates []string
		for _, table := range []string{"channel_model_pricing", "channel_pricing_intervals", "channel_account_stats_model_pricing", "channel_account_stats_pricing_intervals"} {
			predicates = append(predicates, `EXISTS(SELECT 1 FROM `+table+` WHERE cache_write_12h_price IS NOT NULL)`)
			if dirty[table] != nil {
				dirty["accounts"] = map[string]bool{"*": true}
			}
		}
		if err = tx.QueryRowContext(ctx, `SELECT `+strings.Join(predicates, ` OR `)).Scan(&legacyPricingUnsupported); err != nil {
			return false, err
		}
	}
	// Group platform and fallback edits also affect routing for restricted
	// users. Avoid rebuilding every key on an ordinary group update.
	if !full && dirty["groups"] != nil && !dirty["api_keys"]["*"] {
		var keyIDs pq.Int64Array
		if err = tx.QueryRowContext(ctx, `SELECT ARRAY(SELECT k.id FROM api_keys k
			JOIN content_moderation_user_state s ON s.user_id=k.user_id
			WHERE s.openai_blocked AND k.deleted_at IS NULL)`).Scan(&keyIDs); err != nil {
			return false, err
		}
		if dirty["api_keys"] == nil {
			dirty["api_keys"] = map[string]bool{}
		}
		for _, id := range keyIDs {
			dirty["api_keys"][strconv.FormatInt(id, 10)] = true
		}
	}
	if dirty["account_performance_monitors"] != nil || dirty["ai_ops_controls"] != nil {
		dirty["accounts"] = map[string]bool{"*": true}
	}
	// TLS profile selection can change inside account.extra; profiles are small.
	if dirty["accounts"] != nil {
		dirty["tls_fingerprint_profiles"] = map[string]bool{"*": true}
	}
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE site_projection_stage(table_name text,row_key text,value jsonb,PRIMARY KEY(table_name,row_key)) ON COMMIT DROP`); err != nil {
		return false, err
	}
	changed := false
	for _, t := range s.tables {
		if siteLegacySchemas[s.projectionSchema(p)] && t.name == "account_performance_monitors" {
			continue
		}
		keys := dirty[t.name]
		if !full && len(keys) == 0 {
			continue
		}
		all := full || keys["*"]
		where := s.siteFilter(t)
		if t.name == "settings" && siteLegacySchemas[s.projectionSchema(p)] {
			for _, key := range service.SiteOptionalSettingKeys {
				where += " AND key<>" + pq.QuoteLiteral(key)
			}
		}
		var ids []string
		if !all {
			for key := range keys {
				ids = append(ids, key)
			}
			// Numeric comparisons preserve the primary-key index on large tables.
			where += ` AND t."` + t.keys[0] + `"=ANY($1::bigint[])`
		}
		projection := siteProjectionSQL(t.name, legacy)
		switch t.name {
		case "users":
			// Older replicas already reject inactive users in gateway auth.
			projection += ` || CASE WHEN EXISTS (SELECT 1 FROM content_moderation_user_state s
				WHERE s.user_id=t.id AND s.all_platforms_blocked)
				THEN jsonb_build_object('status','disabled') ELSE '{}'::jsonb END`
		case "api_keys":
			// The primary resolves mixed groups and fallbacks. Withhold the
			// routing digest for these keys while an OpenAI restriction applies.
			projection += ` || CASE WHEN EXISTS (SELECT 1 FROM content_moderation_user_state s
				WHERE s.user_id=t.user_id AND s.openai_blocked)
				AND (t.group_id IS NULL OR EXISTS (
					SELECT 1 FROM groups g
					WHERE g.id IN (SELECT t.group_id UNION ALL SELECT kg.group_id FROM api_key_groups kg WHERE kg.api_key_id=t.id)
					AND (g.platform IN ('openai','composite','') OR g.fallback_group_id IS NOT NULL OR g.fallback_group_id_on_invalid_request IS NOT NULL)))
				THEN jsonb_build_object('key',NULL) ELSE '{}'::jsonb END`
		}
		if t.name == "api_keys" && legacy {
			// Redaction uses this permission to remove the legacy routing hint.
			// It is stripped before transmission to replicas that cannot enforce it.
			projection += ` || jsonb_build_object('context_token_limit',t.context_token_limit)`
		}
		if t.name == "accounts" {
			projection += ` || jsonb_build_object('schedulable',t.schedulable AND NOT EXISTS(SELECT 1 FROM account_performance_monitors m WHERE m.account_id=t.id AND m.paused) AND NOT EXISTS(SELECT 1 FROM ai_ops_controls a WHERE a.account_id=t.id AND a.ai_paused))`
			if legacyPricingUnsupported {
				projection += ` || CASE WHEN t.platform='openai' THEN jsonb_build_object('schedulable',false,'extra',COALESCE(t.extra,'{}'::jsonb)||'{"site_secondary_enabled":false}'::jsonb) ELSE '{}'::jsonb END`
			}
		}
		args := []any{}
		if !all {
			args = append(args, pq.Array(ids))
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+projection+` FROM "`+t.name+`" t WHERE `+where, args...)
		if err != nil {
			return false, err
		}
		var selected []site.Row
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return false, err
			}
			value, e := redactSiteRow(t.name, raw, p)
			if e != nil {
				_ = rows.Close()
				return false, e
			}
			if t.name == "settings" {
				value, e = s.projectSiteSetting(value, false)
				if e != nil {
					_ = rows.Close()
					return false, e
				}
			}
			key, e := siteRowKey(t, value)
			if e != nil {
				_ = rows.Close()
				return false, e
			}
			selected = append(selected, site.Row{Table: t.name, Key: key, Value: value})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return false, err
		}
		if err = s.stageProjection(ctx, tx, selected); err != nil {
			return false, err
		}
		// Tombstones are retained for disconnected cursors; only scoped rows were
		// ever materialized, so revocation does not expose foreign credentials.
		q := `UPDATE site_export_rows r SET value=NULL,revision=$2 WHERE node_id=$1 AND table_name=$3 AND value IS NOT NULL AND NOT EXISTS(SELECT 1 FROM site_projection_stage s WHERE s.table_name=r.table_name AND s.row_key=r.row_key)`
		args = []any{p.ID, revision, t.name}
		if !all {
			q += ` AND row_key=ANY($4)`
			args = append(args, pq.Array(ids))
		}
		result, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return false, err
		}
		n, _ := result.RowsAffected()
		changed = changed || n > 0
	}
	if full || dirty["$settlements"] != nil {
		if err = s.stageSiteSettlements(ctx, tx, p.ID); err != nil {
			return false, err
		}
	}
	// Non-table runtime policy is cheap and stays authoritative on every poll.
	for name, export := range map[string]func() ([]byte, error){"$pricing": s.ExportPricing, "$policy": s.ExportRequestPolicy} {
		if export == nil {
			continue
		}
		raw, e := export()
		if e == nil && name == "$policy" && siteLegacySchemas[s.projectionSchema(p)] {
			raw, e = config.LegacySiteRequestPolicy(raw)
		}
		if e != nil {
			return false, e
		}
		key := "catalog"
		if name == "$policy" {
			key = "request"
		}
		if err = s.stageProjection(ctx, tx, []site.Row{{Table: name, Key: key, Value: raw}}); err != nil {
			return false, err
		}
	}
	var accounts []int64
	rows, err = tx.QueryContext(ctx, `SELECT id FROM accounts WHERE `+siteAccountGrantSQL)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, err
		}
		accounts = append(accounts, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	var quality []site.Row
	if siteLegacySchemas[s.projectionSchema(p)] {
		quality, err = exportSiteQuality(ctx, tx, accounts)
	} else {
		quality, err = exportSiteCurrentQuality(ctx, tx, accounts)
	}
	if err != nil {
		return false, err
	}
	if err = s.stageProjection(ctx, tx, quality); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE site_export_rows r SET value=NULL,revision=$2 WHERE node_id=$1 AND table_name IN ('$quality','$settlements') AND value IS NOT NULL AND (table_name='$quality' OR $3) AND NOT EXISTS(SELECT 1 FROM site_projection_stage s WHERE s.table_name=r.table_name AND s.row_key=r.row_key)`, p.ID, revision, full || dirty["$settlements"] != nil)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	changed = changed || n > 0
	result, err = tx.ExecContext(ctx, `INSERT INTO site_export_rows(node_id,table_name,row_key,value,revision) SELECT $1,table_name,row_key,value,$2 FROM site_projection_stage ON CONFLICT(node_id,table_name,row_key) DO UPDATE SET value=EXCLUDED.value,revision=EXCLUDED.revision WHERE site_export_rows.value IS DISTINCT FROM EXCLUDED.value`, p.ID, revision)
	if err != nil {
		return false, err
	}
	n, _ = result.RowsAffected()
	return changed || n > 0, nil
}
func (s *SiteStore) stageProjection(ctx context.Context, tx *sql.Tx, rows []site.Row) error {
	for start := 0; start < len(rows); start += 512 {
		raw, err := json.Marshal(rows[start:min(start+512, len(rows))])
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_projection_stage SELECT x."table",x.key,x.value FROM jsonb_to_recordset($1::jsonb) AS x("table" text,key text,value jsonb)`, string(raw)); err != nil {
			return err
		}
	}
	return nil
}
