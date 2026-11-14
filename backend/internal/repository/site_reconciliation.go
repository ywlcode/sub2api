package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

const siteRejectionColumns = `node_id,event_id,kind,reason,state,resolution_note,resolved_at,resolved_by,attempts,created_at,updated_at`

func scanSiteRejection(row interface{ Scan(...any) error }, payload bool) (*service.SiteRejectedEvent, error) {
	r := &service.SiteRejectedEvent{}
	args := []any{&r.NodeID, &r.EventID, &r.Kind, &r.Reason, &r.State, &r.ResolutionNote, &r.ResolvedAt, &r.ResolvedBy, &r.Attempts, &r.CreatedAt, &r.UpdatedAt}
	if payload {
		args = append(args, &r.Payload)
	}
	err := row.Scan(args...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrSiteRejectionNotFound
	}
	return r, err
}

func (s *SiteStore) ListSiteRejections(ctx context.Context, node, state string, after int64, limit int) ([]service.SiteRejectedEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+siteRejectionColumns+` FROM site_rejected_events WHERE node_id=$1 AND ($2='' OR state=$2) AND event_id>$3 ORDER BY event_id LIMIT $4`, node, state, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []service.SiteRejectedEvent{}
	for rows.Next() {
		r, e := scanSiteRejection(rows, false)
		if e != nil {
			return nil, e
		}
		result = append(result, *r)
	}
	return result, rows.Err()
}

func (s *SiteStore) GetSiteRejection(ctx context.Context, node string, id int64) (*service.SiteRejectedEvent, error) {
	return scanSiteRejection(s.db.QueryRowContext(ctx, `SELECT `+siteRejectionColumns+`,payload FROM site_rejected_events WHERE node_id=$1 AND event_id=$2`, node, id), true)
}

// "settled" acknowledges an independently verified manual adjustment; it does
// not charge again. "waived" explicitly releases the reservation. "retry"
// imports the immutable original bill through normal idempotent accounting.
func (s *SiteStore) ResolveSiteRejection(ctx context.Context, node string, id int64, action, note string, actor int64) (*service.SiteRejectedEvent, error) {
	if s.cfg.EffectiveMode() != site.Primary {
		return nil, apperrors.New(400, "SITE_NOT_PRIMARY", "Reconciliation is primary-only")
	}
	note = strings.TrimSpace(note)
	if id <= 0 || note == "" || len(note) > 2000 || (action != "retry" && action != "settled" && action != "waived") {
		return nil, apperrors.New(400, "INVALID_RECONCILIATION", "Specify retry, settled or waived and a reconciliation note")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('site-delivery:'||$1,0))`, node); err != nil {
		return nil, err
	}
	r, err := scanSiteRejection(tx.QueryRowContext(ctx, `SELECT `+siteRejectionColumns+`,payload FROM site_rejected_events WHERE node_id=$1 AND event_id=$2 FOR UPDATE`, node, id), true)
	if err != nil {
		return nil, err
	}
	if r.State == "settled" || r.State == "waived" {
		if action == r.State || action == "retry" && r.State == "settled" {
			return r, nil
		}
		return nil, service.ErrSiteReconciliationConflict
	}
	var imported []service.UsageBillingEvent
	if action == "retry" {
		var digest string
		err = tx.QueryRowContext(ctx, `SELECT digest FROM site_receipts WHERE node_id=$1 AND event_id=$2`, node, id).Scan(&digest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM site_receipts WHERE node_id=$1 AND event_id=$2 AND rejection<>''`, node, id); err != nil {
			return nil, err
		}
		bill, e := s.importEvent(ctx, tx, site.Peer{ID: node}, site.Event{ID: id, Kind: r.Kind, Payload: r.Payload})
		if e != nil {
			if !permanentSiteEventError(e) && !retryableSiteEvent(e) {
				return nil, e
			}
			return nil, service.ErrSiteReconciliationBlocked.WithCause(e)
		}
		if digest != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE site_receipts SET digest=$3 WHERE node_id=$1 AND event_id=$2`, node, id, digest); err != nil {
				return nil, err
			}
		}
		if bill != nil {
			imported = append(imported, *bill)
		}
		action = "settled"
	} else {
		digest := site.EventDigest(site.Event{Kind: r.Kind, Payload: r.Payload})
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_receipts(node_id,event_id,digest,rejection) VALUES($1,$2,$3,$4) ON CONFLICT(node_id,event_id) DO NOTHING`, node, id, digest, r.Reason); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE site_rejected_events SET state=$3,resolution_note=$4,resolved_at=$5,resolved_by=NULLIF($6,0),updated_at=$5 WHERE node_id=$1 AND event_id=$2`, node, id, action, note, now, actor); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE site_receipts SET rejection='' WHERE node_id=$1 AND event_id=$2`, node, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.afterSiteImport(ctx, imported)
	r.State, r.ResolutionNote, r.ResolvedAt, r.UpdatedAt = action, note, &now, now
	if actor > 0 {
		r.ResolvedBy = &actor
	}
	return r, nil
}

// Only terminal metadata is projected. It shares the exact snapshot with the
// authoritative balance so release and payment can never be installed apart.
func (s *SiteStore) stageSiteSettlements(ctx context.Context, tx *sql.Tx, node string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO site_projection_stage(table_name,row_key,value)
 SELECT '$settlements',event_id::text,jsonb_build_object('state',q.state,'note',q.resolution_note,'at',q.resolved_at,'by',q.resolved_by,'digest',r.digest)
 FROM site_rejected_events q JOIN site_receipts r USING(node_id,event_id) WHERE node_id=$1 AND q.state IN ('settled','waived') AND q.confirmed_at IS NULL`, node)
	return err
}

func (s *SiteStore) installSiteSettlements(ctx context.Context, tx *sql.Tx, rows []site.Row) error {
	for _, r := range rows {
		if r.Table != "$settlements" || len(r.Value) == 0 {
			continue
		}
		id, err := strconv.ParseInt(r.Key, 10, 64)
		var settled struct {
			Digest string    `json:"digest"`
			State  string    `json:"state"`
			Note   string    `json:"note"`
			At     time.Time `json:"at"`
			By     *int64    `json:"by"`
		}
		if err != nil || id <= 0 || json.Unmarshal(r.Value, &settled) != nil || (settled.State != "settled" && settled.State != "waived") || settled.At.IsZero() {
			return errors.New("invalid site settlement metadata")
		}
		if settled.Digest != "" {
			var event site.Event
			err := tx.QueryRowContext(ctx, `SELECT event_id,kind,payload FROM (SELECT id event_id,kind,payload FROM site_outbox UNION ALL SELECT event_id,kind,payload FROM site_rejected_events WHERE node_id=$2) b WHERE event_id=$1 LIMIT 1`, id, s.cfg.ID).Scan(&event.ID, &event.Kind, &event.Payload)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && site.EventDigest(event) != settled.Digest {
				continue
			}
		}
		// The resolution can precede a lost upload response. Preserve evidence
		// directly from outbox before removing it, including deferred bills.
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_rejected_events(node_id,event_id,kind,payload,reason) SELECT $1,id,kind,payload,'Primary reconciliation' FROM site_outbox WHERE id=$2 ON CONFLICT(node_id,event_id) DO NOTHING`, s.cfg.ID, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE site_rejected_events SET state=$3,resolution_note=$4,resolved_at=$5,resolved_by=$6,updated_at=NOW() WHERE node_id=$1 AND event_id=$2`, s.cfg.ID, id, settled.State, settled.Note, settled.At, settled.By); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM site_outbox WHERE id=$1`, id); err != nil {
			return err
		}
	}
	return nil
}

// A client only echoes a version after its complete snapshot is installed.
// Retire settlement projections then, retaining operator evidence for 90 days.
func (s *SiteStore) confirmSiteSettlements(ctx context.Context, conn *sql.Conn, node, version string) error {
	parts := strings.Split(version, ":")
	if len(parts) != 2 {
		return nil
	}
	revision, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || revision <= 0 {
		return nil
	}
	_, err = conn.ExecContext(ctx, `UPDATE site_rejected_events q SET confirmed_at=NOW()
 FROM site_export_nodes n,site_export_rows r WHERE q.node_id=$1 AND n.node_id=q.node_id
 AND n.epoch=$2 AND n.revision>=$3 AND r.node_id=q.node_id AND r.table_name='$settlements'
 AND r.row_key=q.event_id::text AND r.revision<=$3 AND r.value IS NOT NULL
 AND q.state IN ('settled','waived') AND q.confirmed_at IS NULL`, node, parts[0], revision)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `WITH expired AS (SELECT node_id,event_id FROM site_rejected_events
 WHERE node_id=$1 AND confirmed_at<NOW()-INTERVAL '90 days' AND state IN ('settled','waived')
 ORDER BY event_id LIMIT 512 FOR UPDATE SKIP LOCKED)
 DELETE FROM site_rejected_events q USING expired e WHERE q.node_id=e.node_id AND q.event_id=e.event_id`, node)
	return err
}
