//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteReviewSecondaryRetentionProtectsUnsettledMoney(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	settled, pending, rejected := p.bill(t), p.bill(t), p.bill(t)
	_, err := p.primary.Deliver(ctx, p.peer, []site.Event{settled})
	require.NoError(t, err)
	p.sync(t)
	require.NoError(t, p.local.MarkDelivered(ctx, site.DeliveryResponse{Rejected: []site.Rejection{{ID: rejected.ID, Reason: "manual reconciliation required"}}}))
	_, err = p.replica.Exec(`UPDATE usage_billing_events SET state='complete',completed_at=now()-interval '100 days';
UPDATE usage_logs SET created_at=now()-interval '100 days';
UPDATE usage_billing_dedup SET created_at=now()-interval '100 days'`)
	require.NoError(t, err)
	_, err = p.replica.Exec(`INSERT INTO ops_error_logs(api_key_id,request_id,error_phase,error_type,error_message,created_at) VALUES($1,'old-diagnostic','upstream','timeout','test',now()-interval '100 days')`, p.key)
	require.NoError(t, err)
	cutoff := time.Now().AddDate(0, 0, -30)
	policy := service.SiteHistoryRetentionPolicy{Usage: cutoff, BillingEvents: cutoff, Dedup: cutoff, ErrorLogs: cutoff}
	require.NoError(t, p.local.PruneSiteHistory(ctx, policy))
	for _, table := range []string{"usage_logs", "usage_billing_events", "usage_billing_dedup"} {
		var count int
		require.NoError(t, p.replica.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 2, count, table+" must retain pending and rejected bills only")
	}
	var count int
	require.NoError(t, p.replica.QueryRow(`SELECT count(*) FROM ops_error_logs WHERE request_id='old-diagnostic'`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, p.replica.QueryRow(`SELECT count(*) FROM site_outbox`).Scan(&count))
	require.Equal(t, 2, count, "pending bill and diagnostic payload must remain durable")
	require.NoError(t, p.replica.QueryRow(`SELECT count(*) FROM site_rejected_events`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = p.primary.Deliver(ctx, p.peer, []site.Event{pending, pending})
	require.NoError(t, err)
	p.sync(t)
	p.balance(t, "98.00000000", "97.00000000")
}

func TestSiteReviewProjectionTombstoneRetention(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	base := p.version
	_, err := p.db.Exec(`UPDATE accounts SET extra='{}' WHERE id=$1`, p.account)
	require.NoError(t, err)
	delta, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: base})
	require.NoError(t, err)
	require.False(t, delta.Full)
	var count int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_export_rows WHERE value IS NULL`).Scan(&count))
	require.Positive(t, count)
	_, release, err := p.primary.projectionConnection(ctx, p.peer.ID)
	require.NoError(t, err)
	cleanupCtx, cancel := context.WithTimeout(ctx, time.Second)
	err = p.primary.PruneSiteHistory(cleanupCtx, service.SiteHistoryRetentionPolicy{})
	cancel()
	release()
	require.NoError(t, err, "cleanup must skip an active publication without waiting")
	_, err = p.db.Exec(`ALTER TABLE site_export_nodes ADD CONSTRAINT review_fail_floor CHECK(retained_revision=0)`)
	require.NoError(t, err)
	require.Error(t, p.primary.PruneSiteHistory(ctx, service.SiteHistoryRetentionPolicy{}))
	var remaining int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_export_rows WHERE value IS NULL`).Scan(&remaining))
	require.Equal(t, count, remaining, "tombstone deletion and retained cursor floor must commit atomically")
	_, err = p.db.Exec(`ALTER TABLE site_export_nodes DROP CONSTRAINT review_fail_floor`)
	require.NoError(t, err)
	require.NoError(t, p.primary.PruneSiteHistory(ctx, service.SiteHistoryRetentionPolicy{}))
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_export_rows WHERE value IS NULL`).Scan(&count))
	require.Zero(t, count)
	behind, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: base})
	require.NoError(t, err)
	require.True(t, behind.Full, "old cursor must reinstall after its tombstones are reclaimed")
	_, err = p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: base, Cursor: "accounts:1", TargetVersion: delta.Version})
	require.Error(t, err, "do not resume an old delta page as a partial full snapshot")
	caughtUp, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: delta.Version})
	require.NoError(t, err)
	require.False(t, caughtUp.Full)
	require.Empty(t, caughtUp.Rows)
	require.NoError(t, p.local.Install(ctx, site.SyncResponse{PrimaryID: "main", Version: behind.Version, Full: true, Rows: behind.Rows}))
	require.NoError(t, p.replica.QueryRow(`SELECT count(*) FROM accounts WHERE deleted_at IS NULL`).Scan(&count))
	require.Zero(t, count, "revoked accounts must not remain active after a reclaimed tombstone")
	require.NoError(t, p.replica.QueryRow(`SELECT count(*) FROM site_replica_rows WHERE table_name='accounts'`).Scan(&count))
	require.Zero(t, count, "revoked accounts must leave the local router")
}

func TestSiteReviewDiagnosticRetentionKeepsBillingReceipts(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	bill := p.bill(t)
	var events []site.Event
	for _, age := range []string{"100 days", "1 day"} {
		var raw []byte
		require.NoError(t, p.replica.QueryRow(`INSERT INTO ops_error_logs(request_id,api_key_id,error_phase,error_type,error_message,created_at) VALUES($1,$2,'upstream','timeout','test',now()-$3::interval) RETURNING to_jsonb(ops_error_logs)`, age, p.key, age).Scan(&raw))
		events = append(events, site.Event{ID: int64(900001 + len(events)), Kind: "ops_error_logs", Payload: json.RawMessage(raw)})
	}
	events = append(events, bill)
	out, err := p.primary.Deliver(ctx, p.peer, events)
	require.NoError(t, err)
	require.Len(t, out.Acknowledged, 3)
	_, err = p.db.Exec(`DELETE FROM ops_error_logs WHERE created_at<now()-interval '30 days'; UPDATE site_receipts SET received_at=now()-interval '100 days'`)
	require.NoError(t, err)
	policy := service.SiteHistoryRetentionPolicy{ErrorLogs: time.Now().AddDate(0, 0, -30)}
	require.NoError(t, p.primary.PruneSiteHistory(ctx, policy))
	var count int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_receipts`).Scan(&count))
	require.Equal(t, 2, count, "billing and retained diagnostic receipts survive")
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_imported_logs`).Scan(&count))
	require.Equal(t, 1, count)
	out, err = p.primary.Deliver(ctx, p.peer, events)
	require.NoError(t, err)
	require.Len(t, out.Acknowledged, 3)
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM ops_error_logs`).Scan(&count))
	require.Equal(t, 1, count, "replayed expired diagnostics must not resurrect deleted history")
	p.balance(t, "99.00000000", "99.00000000")
}
