package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/lib/pq"
)

// A delivery transaction is independent of the authorization lease. PostgreSQL
// serializes a peer across primary instances, including lost-response retries.
func (s *SiteStore) Deliver(ctx context.Context, p site.Peer, events []site.Event) (site.DeliveryResponse, error) {
	out := site.DeliveryResponse{Protocol: site.ProtocolVersion, NodeID: p.ID}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('site-delivery:'||$1,0))`, p.ID); err != nil {
		return out, err
	}
	imported, err := s.importEvents(ctx, tx, p, events, &out)
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.afterSiteImport(ctx, imported)
	return out, nil
}

func (s *SiteStore) afterSiteImport(ctx context.Context, events []service.UsageBillingEvent) {
	if s.AfterImport != nil && len(events) > 0 {
		if err := s.AfterImport(ctx, events); err != nil {
			// Canonical bills and receipts are already committed. The existing durable
			// billing recovery worker retries cache invalidation from state='applied'.
			slog.Error("site billing committed; cache invalidation deferred to recovery", "events", len(events), "error", err)
		}
	}
}

func (s *SiteStore) importEvents(ctx context.Context, tx *sql.Tx, p site.Peer, events []site.Event, out *site.DeliveryResponse) ([]service.UsageBillingEvent, error) {
	var imported []service.UsageBillingEvent
	for _, e := range events {
		var rejection string
		err := tx.QueryRowContext(ctx, `SELECT rejection FROM site_receipts WHERE node_id=$1 AND event_id=$2`, p.ID, e.ID).Scan(&rejection)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if rejection != "" {
			out.Rejected = append(out.Rejected, site.Rejection{ID: e.ID, Reason: rejection})
			continue
		}
		if _, err = tx.ExecContext(ctx, `SAVEPOINT site_event`); err != nil {
			return nil, err
		}
		bill, err := s.importEvent(ctx, tx, p, e)
		if err != nil {
			if !permanentSiteEventError(err) {
				return nil, err
			}
			if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT site_event`); rollbackErr != nil {
				return nil, rollbackErr
			}
			reason := err.Error()
			if len(reason) > 2048 {
				reason = reason[:2048]
			}
			digest := sha256.Sum256(append([]byte(e.Kind+"\x00"), e.Payload...))
			if _, err = tx.ExecContext(ctx, `INSERT INTO site_rejected_events(node_id,event_id,kind,payload,reason) VALUES($1,$2,$3,$4::jsonb,$5) ON CONFLICT(node_id,event_id) DO NOTHING`, p.ID, e.ID, e.Kind, string(e.Payload), reason); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO site_receipts(node_id,event_id,digest,rejection) VALUES($1,$2,$3,$4) ON CONFLICT(node_id,event_id) DO NOTHING`, p.ID, e.ID, hex.EncodeToString(digest[:]), reason); err != nil {
				return nil, err
			}
			out.Rejected = append(out.Rejected, site.Rejection{ID: e.ID, Reason: reason})
			slog.Error("site event quarantined; reconciliation required", "node", p.ID, "event_id", e.ID, "kind", e.Kind, "reason", reason)
		} else {
			out.Acknowledged = append(out.Acknowledged, e.ID)
			if bill != nil {
				imported = append(imported, *bill)
			}
		}
		if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT site_event`); err != nil {
			return nil, err
		}
	}
	return imported, nil
}

func permanentSiteEventError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pg *pq.Error
	if errors.As(err, &pg) {
		return pg.Code.Class() == "22" || pg.Code.Class() == "23"
	}
	// Repository validation errors are marked explicitly. Unknown I/O failures
	// must retry and may never be acknowledged as permanently invalid data.
	var invalid *siteEventValidationError
	return errors.As(err, &invalid) || errors.Is(err, service.ErrAccountNotFound) || errors.Is(err, service.ErrUsageBillingRequestConflict) || errors.Is(err, service.ErrUsageBillingEventVersion)
}

type siteEventValidationError struct{ error }

func invalidSiteEvent(err error) error { return &siteEventValidationError{err} }

func (s *SiteStore) MarkDelivered(ctx context.Context, out site.DeliveryResponse) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE site_outbox SET delivered=true WHERE id=ANY($1) AND kind='billing'`, pq.Array(out.Acknowledged)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM site_outbox WHERE id=ANY($1) AND kind<>'billing'`, pq.Array(out.Acknowledged)); err != nil {
		return err
	}
	if err = s.quarantineLocal(ctx, tx, out.Rejected); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *SiteStore) quarantineLocal(ctx context.Context, tx *sql.Tx, rejected []site.Rejection) error {
	for _, r := range rejected {
		if _, err := tx.ExecContext(ctx, `INSERT INTO site_rejected_events(node_id,event_id,kind,payload,reason) SELECT $1,id,kind,payload,$3 FROM site_outbox WHERE id=$2 ON CONFLICT(node_id,event_id) DO NOTHING`, s.cfg.ID, r.ID, r.Reason); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM site_outbox WHERE id=$1`, r.ID); err != nil {
			return err
		}
		slog.Error("site rejected event retained locally", "event_id", r.ID, "reason", r.Reason)
	}
	return nil
}

func (s *SiteStore) pendingDelivery(ctx context.Context, limit int) ([]site.Event, error) {
	if limit <= 0 || limit > site.DeliveryBatch {
		limit = site.DeliveryBatch
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Fair diagnostics share; bills cannot be starved by anonymous/failed traffic.
	rows, err := tx.QueryContext(ctx, `WITH waiting AS (SELECT count(*) n FROM site_outbox WHERE kind='billing' AND attempted),
 retry AS (SELECT id FROM site_outbox WHERE kind='billing' AND attempted AND NOT delivered ORDER BY id LIMIT $1),
 fresh AS (SELECT id FROM site_outbox WHERE kind='billing' AND NOT attempted ORDER BY id LIMIT GREATEST(0,LEAST($1-(SELECT count(*) FROM retry),$2-(SELECT n FROM waiting)))),
 diagnostics AS (SELECT id FROM site_outbox WHERE kind<>'billing' AND NOT delivered ORDER BY id LIMIT $3-(SELECT count(*) FROM retry)-(SELECT count(*) FROM fresh)),
 chosen AS (SELECT id FROM retry UNION ALL SELECT id FROM fresh UNION ALL SELECT id FROM diagnostics)
 UPDATE site_outbox o SET attempted=true FROM chosen c WHERE o.id=c.id RETURNING o.id,o.kind,o.payload`, max(1, limit*3/4), site.MaxPendingBills, limit)
	if err != nil {
		return nil, err
	}
	var events []site.Event
	for rows.Next() {
		var e site.Event
		if err = rows.Scan(&e.ID, &e.Kind, &e.Payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		events = append(events, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("prepare site delivery: %w", err)
	}
	sort.Slice(events, func(i, j int) bool {
		if (events[i].Kind == "billing") != (events[j].Kind == "billing") {
			return events[i].Kind == "billing"
		}
		return events[i].ID < events[j].ID
	})
	return events, nil
}

// Legacy v3 queues may exceed the bounded v4 outstanding window. Reconcile them
// in pages while closed; normal v4 delivery never creates more than this limit.
func (s *SiteStore) PendingCoverage(ctx context.Context) (bool, error) {
	var more bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM site_outbox WHERE kind='billing' AND attempted ORDER BY id OFFSET 4096 LIMIT 1)`).Scan(&more)
	return !more, err
}
