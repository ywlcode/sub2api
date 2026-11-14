package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/lib/pq"
)

func (s *SiteStore) PendingReceipts(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,payload FROM site_outbox WHERE kind='billing' AND attempted ORDER BY id LIMIT 4096`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var e site.Event
		if err = rows.Scan(&e.ID, &e.Kind, &e.Payload); err != nil {
			return nil, err
		}
		out[e.ID] = site.EventDigest(e)
	}
	return out, rows.Err()
}

func (s *SiteStore) validateLocalReceipts(ctx context.Context, tx *sql.Tx, digests map[int64]string, accepted []int64, rejected []site.Rejection) error {
	ids := append([]int64(nil), accepted...)
	for _, r := range rejected {
		ids = append(ids, r.ID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,kind,payload FROM site_outbox WHERE id=ANY($1)`, pq.Array(ids))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e site.Event
		if err = rows.Scan(&e.ID, &e.Kind, &e.Payload); err != nil {
			return err
		}
		if digests[e.ID] == "" || digests[e.ID] != site.EventDigest(e) {
			return errors.New("site receipt payload mismatch")
		}
	}
	return rows.Err()
}
