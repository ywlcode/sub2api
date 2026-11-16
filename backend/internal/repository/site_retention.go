package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

func (s *SiteStore) PruneSiteHistory(ctx context.Context, policy service.SiteHistoryRetentionPolicy) error {
	if s.cfg.EffectiveMode() == site.Secondary {
		return s.pruneSecondaryHistory(ctx, policy)
	}
	if s.cfg.EffectiveMode() != site.Primary {
		return nil
	}
	var failures []error
	for _, target := range []struct {
		table, column, key, keyType string
		cutoff                      time.Time
	}{
		{"ops_error_logs", "created_at", "id", "bigint", policy.ErrorLogs},
		{"request_connection_traces", "started_at", "trace_id", "uuid", policy.Traces},
	} {
		if target.cutoff.IsZero() {
			continue
		}
		if err := s.pruneSiteDiagnostics(ctx, target.table, target.column, target.key, target.keyType, target.cutoff); err != nil {
			failures = append(failures, fmt.Errorf("%s replication history: %w", target.table, err))
		}
	}
	if err := s.pruneSiteTombstones(ctx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// The watermark is monotonic even if retention is later lengthened. History
// that has already expired cannot be restored by a delayed retry or old backup.
func (s *SiteStore) pruneSiteDiagnostics(ctx context.Context, table, column, key, keyType string, cutoff time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE site_diagnostic_retention SET discarded_before=GREATEST(discarded_before,$2) WHERE table_name=$1`, table, cutoff); err != nil {
		return err
	}
	// Imports hold a shared lock on the watermark until their transaction ends.
	// The update above drains older imports; new ones cannot resurrect this range.
	if _, err := deleteHistoryBatches(ctx, s.db, table, column, "", cutoff); err != nil {
		return err
	}
	// Advance over retained mappings too: repeatedly scanning the same live
	// prefix for each deletion batch becomes quadratic on a busy primary.
	node, source := "", ""
	for {
		err := s.db.QueryRowContext(ctx, `WITH batch AS MATERIALIZED (
 SELECT ctid,node_id,source_id,local_id FROM site_imported_logs WHERE table_name=$1
 AND (node_id,source_id)>($2,$3) ORDER BY node_id,source_id LIMIT 5000
), removed AS (DELETE FROM site_imported_logs WHERE ctid IN (
 SELECT m.ctid FROM batch m WHERE NOT EXISTS(SELECT 1 FROM `+table+` l WHERE l.`+key+`=m.local_id::`+keyType+`)))
SELECT node_id,source_id FROM batch ORDER BY node_id DESC,source_id DESC LIMIT 1`, table, node, source).Scan(&node, &source)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
	}
	_, err := deleteHistoryBatches(ctx, s.db, "site_receipts", "diagnostic_at",
		`AND diagnostic_kind='`+table+`' AND rejection='' AND NOT EXISTS(SELECT 1 FROM site_rejected_events q WHERE q.node_id=site_receipts.node_id AND q.event_id=site_receipts.event_id)`, cutoff)
	return err
}

func (s *SiteStore) pruneSiteTombstones(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT node_id FROM site_export_rows WHERE value IS NULL`)
	if err != nil {
		return err
	}
	var nodes []string
	for rows.Next() {
		var node string
		if err = rows.Scan(&node); err != nil {
			_ = rows.Close()
			return err
		}
		nodes = append(nodes, node)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, node := range nodes {
		for {
			n, err := s.pruneSiteTombstoneBatch(ctx, node)
			if err != nil {
				return err
			}
			if n < 5000 {
				break
			}
		}
	}
	return nil
}

func (s *SiteStore) pruneSiteTombstoneBatch(ctx context.Context, node string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('site-projection:'||$1,0))`, node).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil
	} // Never queue cleanup ahead of an active publication.
	var count, revision int64
	err = tx.QueryRowContext(ctx, `WITH batch AS (
 SELECT ctid FROM site_export_rows WHERE node_id=$1 AND value IS NULL ORDER BY revision LIMIT 5000
), removed AS (DELETE FROM site_export_rows WHERE ctid IN (SELECT ctid FROM batch) RETURNING revision)
SELECT count(*),COALESCE(max(revision),0) FROM removed`, node).Scan(&count, &revision)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE site_export_nodes SET retained_revision=GREATEST(retained_revision,$2) WHERE node_id=$1`, node, revision); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SiteStore) pruneSecondaryHistory(ctx context.Context, p service.SiteHistoryRetentionPolicy) error {
	var failures []error
	usageCleaned := !p.Usage.IsZero()
	for _, target := range []struct {
		table, column string
		cutoff        time.Time
		money         bool
	}{
		{"ops_error_logs", "created_at", p.ErrorLogs, false},
		{"ops_ingress_reject_aggregates", "bucket_start", p.ErrorLogs, false},
		{"ops_system_logs", "created_at", p.SystemLogs, false},
		{"request_connection_traces", "started_at", p.Traces, false},
		{"request_timing_details", "created_at", p.Timings, false},
		{"user_traffic_events", "started_at", p.Traffic, false},
		{"usage_logs", "created_at", p.Usage, true},
		{"usage_billing_events", "completed_at", p.BillingEvents, true},
		{"usage_billing_dedup", "created_at", p.Dedup, true},
		{"usage_billing_dedup_archive", "created_at", p.Dedup, true},
	} {
		if target.cutoff.IsZero() {
			continue
		}
		if (target.table == "usage_billing_dedup" || target.table == "usage_billing_dedup_archive") && !usageCleaned {
			continue
		}
		predicate := ""
		if target.money {
			predicate = siteSettledHistoryPredicate(target.table)
		}
		if target.table == "usage_billing_events" {
			predicate += ` AND state IN ('complete','legacy')`
		}
		if _, err := deleteHistoryBatches(ctx, s.db, target.table, target.column, predicate, target.cutoff); err != nil {
			if target.table == "usage_logs" {
				usageCleaned = false
			}
			failures = append(failures, fmt.Errorf("%s: %w", target.table, err))
		}
	}
	return errors.Join(failures...)
}

func siteSettledHistoryPredicate(table string) string {
	identity := func(alias string) string {
		return alias + `.payload#>>'{command,RequestID}'=` + table + `.request_id AND ` + alias + `.payload#>>'{command,APIKeyID}'=` + table + `.api_key_id::text`
	}
	return ` AND NOT EXISTS(SELECT 1 FROM site_outbox o WHERE kind='billing' AND ` + identity("o") + `)
 AND NOT EXISTS(SELECT 1 FROM site_rejected_events q WHERE kind='billing' AND state NOT IN ('settled','waived') AND ` + identity("q") + `)
 AND NOT EXISTS(SELECT 1 FROM usage_billing_events b WHERE b.request_id=` + table + `.request_id AND b.api_key_id=` + table + `.api_key_id AND b.state NOT IN ('complete','legacy'))`
}

func siteDiagnosticTimestamp(table string, raw []byte) (time.Time, error) {
	var record struct {
		CreatedAt time.Time `json:"created_at"`
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return time.Time{}, err
	}
	at := record.CreatedAt
	if table == "request_connection_traces" {
		at = record.StartedAt
	}
	// Missing optional legacy timestamps retain their receipts permanently.
	return at, nil
}
