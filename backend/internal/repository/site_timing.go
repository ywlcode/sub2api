package repository

import (
	"context"
	"database/sql"
	"strings"
)

// Billing and connection diagnostics can commit or arrive in either order.
// Reconcile from both import paths, in the same transaction as their receipt.
// Original client IDs remain available for diagnostics; the node and key scope
// prevent an unrelated site's retry ID from attaching to this site's bill.
func linkSiteRequestTimings(ctx context.Context, tx *sql.Tx, node, sourceRequestID string, apiKeyID int64) error {
	if sourceRequestID == "" || apiKeyID <= 0 {
		return nil
	}
	billedID := siteRequestID(node, sourceRequestID)
	match, value := "t.request_id=$3", billedID
	if strings.HasPrefix(sourceRequestID, "client:") {
		match, value = "t.client_request_id=$3", strings.TrimPrefix(sourceRequestID, "client:")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail,created_at)
 SELECT u.id,t.trace_id,t.detail,t.started_at
 FROM usage_logs u JOIN request_connection_traces t ON t.api_key_id=u.api_key_id
 WHERE u.request_id=$1 AND u.api_key_id=$2 AND `+match+` AND t.detail->>'site_node_id'=$4
 ON CONFLICT(usage_log_id,trace_id) DO UPDATE SET detail=EXCLUDED.detail
 WHERE request_timing_details.detail IS DISTINCT FROM EXCLUDED.detail`, billedID, apiKeyID, value, node)
	return err
}
