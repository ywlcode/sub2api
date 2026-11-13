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
	"time"

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
	return tx.Commit()
}
func (s *SiteStore) registerProjection(ctx context.Context, p site.Peer) error {
	raw, _ := json.Marshal(struct {
		Schema, Token string
		Bases         map[int64]string
	}{s.schema, p.Token, p.BaseURLs})
	sum := sha256.Sum256(raw)
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_export_nodes(node_id,fingerprint,epoch) VALUES($1,$2,$3) ON CONFLICT(node_id) DO UPDATE SET fingerprint=EXCLUDED.fingerprint,epoch=EXCLUDED.epoch,initialized=false WHERE site_export_nodes.fingerprint<>EXCLUDED.fingerprint`, p.ID, hex.EncodeToString(sum[:]), uuid.NewString())
	return err
}

func (s *SiteStore) ExportPage(ctx context.Context, p site.Peer, input site.SyncRequest) (site.Snapshot, error) {
	return s.exportRetry(ctx, p, input, nil, true)
}
func (s *SiteStore) Export(ctx context.Context, p site.Peer, events []site.Event, ids []int64) (site.Snapshot, error) {
	return s.exportRetry(ctx, p, site.SyncRequest{PendingIDs: ids}, events, false)
}
func (s *SiteStore) exportRetry(ctx context.Context, p site.Peer, input site.SyncRequest, events []site.Event, paged bool) (site.Snapshot, error) {
	if err := s.registerProjection(ctx, p); err != nil {
		return site.Snapshot{}, err
	}
	for attempt := 0; ; attempt++ {
		result, err := s.exportProjection(ctx, p, input, events, paged)
		var pg *pq.Error
		if err == nil || attempt >= 5 || !errors.As(err, &pg) || (pg.Code != "40001" && pg.Code != "40P01") {
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
func (s *SiteStore) exportProjection(ctx context.Context, p site.Peer, input site.SyncRequest, events []site.Event, paged bool) (site.Snapshot, error) {
	out := site.Snapshot{Schema: s.schema}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	var revision int64
	var epoch string
	var initialized bool
	if err = tx.QueryRowContext(ctx, `SELECT epoch,revision,initialized FROM site_export_nodes WHERE node_id=$1 FOR UPDATE`, p.ID).Scan(&epoch, &revision, &initialized); err != nil {
		return out, err
	}
	current := epoch + ":" + strconv.FormatInt(revision, 10)
	if input.Cursor != "" && input.TargetVersion != current {
		return out, errors.New("site snapshot changed; restart paging")
	}
	receipt := site.DeliveryResponse{}
	var importedCount int
	committed := false
	if input.Cursor == "" {
		// Grants survive revocation for requests already in flight.
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_account_grants(node_id,account_id) SELECT $1,id FROM accounts WHERE `+siteAccountGrantSQL+` ON CONFLICT DO NOTHING`, p.ID); err != nil {
			return out, err
		}
		imported, e := s.importEvents(ctx, tx, p, events, &receipt)
		if e != nil {
			return out, e
		}
		importedCount = len(imported)
		// Compatibility callers use this transactional import too. Runtime upload is
		// separate; cache recovery after commit is deliberately non-fatal.
		defer func() {
			if committed && importedCount > 0 {
				s.afterSiteImport(ctx, imported)
			}
		}()
		changed, e := s.refreshProjection(ctx, tx, p, revision+1, !initialized)
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
	out.Full = base <= 0 || base > revision
	if out.Full {
		base = 0
	}
	// Receipts and monetary rows share the repeatable-read snapshot, including
	// accepted uploads whose HTTP response was lost.
	ack, err := tx.QueryContext(ctx, `SELECT event_id,rejection FROM site_receipts WHERE node_id=$1 AND event_id=ANY($2::bigint[]) ORDER BY event_id`, p.ID, pq.Array(input.PendingIDs))
	if err != nil {
		return out, err
	}
	for ack.Next() {
		var id int64
		var reason string
		if err = ack.Scan(&id, &reason); err != nil {
			_ = ack.Close()
			return out, err
		}
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
		importedCount = 0
		return out, err
	}
	committed = true
	return out, nil
}

const siteScopeGroups = `SELECT group_id FROM account_groups WHERE account_id IN (SELECT id FROM accounts WHERE ` + siteAccountGrantSQL + `)`

func siteScopedKeys() string {
	return `SELECT k.id FROM api_keys k WHERE k.deleted_at IS NULL AND (k.group_id IN (` + siteScopeGroups + `) OR EXISTS(SELECT 1 FROM api_key_groups kg WHERE kg.api_key_id=k.id AND kg.group_id IN (` + siteScopeGroups + `)))`
}
func siteScopedUsers() string {
	return `SELECT user_id FROM api_keys WHERE id IN (` + siteScopedKeys() + `)`
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

func (s *SiteStore) refreshProjection(ctx context.Context, tx *sql.Tx, p site.Peer, revision int64, full bool) (bool, error) {
	dirty := map[string]map[string]bool{}
	// DELETE takes row locks. A concurrent update re-enqueues after commit.
	rows, err := tx.QueryContext(ctx, `DELETE FROM site_changes WHERE node_id=$1 RETURNING table_name,row_key`, p.ID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var name, key string
		if err = rows.Scan(&name, &key); err != nil {
			_ = rows.Close()
			return false, err
		}
		if dirty[name] == nil {
			dirty[name] = map[string]bool{}
		}
		dirty[name][key] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	full = full || dirty["$scope"] != nil
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
		keys := dirty[t.name]
		if !full && len(keys) == 0 {
			continue
		}
		all := full || keys["*"]
		where := s.siteFilter(t)
		var ids []string
		if !all {
			for key := range keys {
				ids = append(ids, key)
			}
			// Numeric comparisons preserve the primary-key index on large tables.
			where += ` AND t."` + t.keys[0] + `"=ANY($1::bigint[])`
		}
		projection := "to_jsonb(t)"
		if t.name == "accounts" {
			projection += ` || jsonb_build_object('schedulable',t.schedulable AND NOT EXISTS(SELECT 1 FROM account_performance_monitors m WHERE m.account_id=t.id AND m.paused) AND NOT EXISTS(SELECT 1 FROM ai_ops_controls a WHERE a.account_id=t.id AND a.ai_paused))`
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
	// Non-table runtime policy is cheap and stays authoritative on every poll.
	for name, export := range map[string]func() ([]byte, error){"$pricing": s.ExportPricing, "$policy": s.ExportRequestPolicy} {
		if export == nil {
			continue
		}
		raw, e := export()
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
	quality, err := exportSiteQuality(ctx, tx, accounts)
	if err != nil {
		return false, err
	}
	if err = s.stageProjection(ctx, tx, quality); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE site_export_rows r SET value=NULL,revision=$2 WHERE node_id=$1 AND table_name='$quality' AND value IS NOT NULL AND NOT EXISTS(SELECT 1 FROM site_projection_stage s WHERE s.table_name=r.table_name AND s.row_key=r.row_key)`, p.ID, revision)
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
