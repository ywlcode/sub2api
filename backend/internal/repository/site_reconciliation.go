package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
		digest := sha256.Sum256(append([]byte(r.Kind+"\x00"), r.Payload...))
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_receipts(node_id,event_id,digest,rejection) VALUES($1,$2,$3,$4) ON CONFLICT(node_id,event_id) DO NOTHING`, node, id, hex.EncodeToString(digest[:]), r.Reason); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE site_rejected_events SET state=$3,resolution_note=$4,resolved_at=$5,resolved_by=NULLIF($6,0),updated_at=$5 WHERE node_id=$1 AND event_id=$2`, node, id, action, note, now, actor); err != nil {
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
 SELECT '$settlements',event_id::text,jsonb_build_object('state',state,'note',resolution_note,'at',resolved_at,'by',resolved_by)
 FROM site_rejected_events WHERE node_id=$1 AND state IN ('settled','waived')`, node)
	return err
}

func (s *SiteStore) installSiteSettlements(ctx context.Context, tx *sql.Tx, rows []site.Row) error {
	for _, r := range rows {
		if r.Table != "$settlements" {
			continue
		}
		id, err := strconv.ParseInt(r.Key, 10, 64)
		var settled struct {
			State string    `json:"state"`
			Note  string    `json:"note"`
			At    time.Time `json:"at"`
			By    *int64    `json:"by"`
		}
		if err != nil || id <= 0 || json.Unmarshal(r.Value, &settled) != nil || (settled.State != "settled" && settled.State != "waived") || settled.At.IsZero() {
			return errors.New("invalid site settlement metadata")
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
