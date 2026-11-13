package repository

import (
	"context"
	"database/sql"
	"encoding/json"
)

type siteChangeGeneration struct {
	Table      string `json:"table"`
	Key        string `json:"key"`
	Generation int64  `json:"generation"`
}

// The change tokens, business rows and receipts are read from one MVCC
// snapshot. No lock is taken on the queue while constructing the projection.
func readSiteChanges(ctx context.Context, tx *sql.Tx, node string) ([]siteChangeGeneration, error) {
	rows, err := tx.QueryContext(ctx, `SELECT table_name,row_key,generation FROM site_changes WHERE node_id=$1`, node)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var changes []siteChangeGeneration
	for rows.Next() {
		var c siteChangeGeneration
		if err = rows.Scan(&c.Table, &c.Key, &c.Generation); err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// Short READ COMMITTED statements after publication. An in-flight writer is
// skipped; a newer token is retained. A crash at any point only repeats work.
// Sequence values are compared for equality, never used to skip late commits.
func acknowledgeSiteChanges(ctx context.Context, conn *sql.Conn, node string, changes []siteChangeGeneration) error {
	for start := 0; start < len(changes); start += 512 {
		raw, err := json.Marshal(changes[start:min(start+512, len(changes))])
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `WITH candidates AS (
 SELECT c.node_id,c.table_name,c.row_key,c.generation FROM site_changes c
 JOIN jsonb_to_recordset($2::jsonb) x("table" text,key text,generation bigint)
 ON c.table_name=x."table" AND c.row_key=x.key AND c.generation=x.generation
 WHERE c.node_id=$1 FOR UPDATE OF c SKIP LOCKED)
 DELETE FROM site_changes c USING candidates x WHERE c.node_id=x.node_id
 AND c.table_name=x.table_name AND c.row_key=x.row_key AND c.generation=x.generation`, node, string(raw))
		if err != nil {
			return err
		}
	}
	return nil
}
