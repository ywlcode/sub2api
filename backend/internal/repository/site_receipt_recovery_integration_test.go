//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteDeliveredReceiptLossRetransmitsWithoutDoubleCharge(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	e := p.bill(t)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	_, err = p.db.Exec(`DELETE FROM site_receipts WHERE node_id=$1 AND event_id=$2`, p.peer.ID, e.ID)
	require.NoError(t, err)
	p.sync(t)
	ready, err := p.local.Pending(ctx, site.DeliveryBatch)
	require.NoError(t, err)
	require.Empty(t, ready, "normal snapshot lag must not create a retry storm")
	_, err = p.replica.Exec(`UPDATE site_outbox SET delivered_at=NOW()-INTERVAL '3 minutes' WHERE id=$1`, e.ID)
	require.NoError(t, err)
	p.sync(t)
	ready, err = p.local.Pending(ctx, site.DeliveryBatch)
	require.NoError(t, err)
	require.Len(t, ready, 1)
	r, err = p.primary.Deliver(ctx, p.peer, ready)
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
	var bills int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM usage_logs WHERE user_id=$1`, p.user).Scan(&bills))
	require.Equal(t, 1, bills)
}

func TestSiteFreshDatabaseCannotConsumePreviousDatabaseReceipt(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	old := p.bill(t)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{old})
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	p.sync(t)
	// Real fresh database, same configured peer and token, restarted sequence.
	p.replica, _ = newBillingAuditDatabase(t)
	p.local, err = NewSiteStore(p.replica, &config.Config{Site: site.Config{Mode: site.Secondary, ID: p.peer.ID, Token: p.peer.Token}})
	require.NoError(t, err)
	require.NotEmpty(t, p.local.ReplicaID())
	p.peer.ReplicaID = p.local.ReplicaID()
	p.version = ""
	p.sync(t)
	fresh := p.bill(t)
	require.Equal(t, old.ID, fresh.ID)
	snap := p.sync(t)
	require.Empty(t, snap.Acknowledged)
	p.balance(t, "99.00000000", "98.00000000")
	r, err = p.primary.Deliver(ctx, p.peer, []site.Event{fresh})
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	p.sync(t)
	p.balance(t, "98.00000000", "98.00000000")
	var bills int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM usage_logs WHERE user_id=$1`, p.user).Scan(&bills))
	require.Equal(t, 2, bills)
	reloaded, err := NewSiteStore(p.replica, &config.Config{Site: site.Config{Mode: site.Secondary, ID: p.peer.ID, Token: p.peer.Token}})
	require.NoError(t, err)
	require.Equal(t, p.local.ReplicaID(), reloaded.ReplicaID(), "ordinary restart/migration retains identity")
}

func TestSiteReceiptDigestRejectsReusedIDWithinDatabase(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	old := p.bill(t)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{old})
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	p.sync(t)
	fresh := p.bill(t)
	_, err = p.replica.Exec(`UPDATE site_outbox SET id=$1 WHERE id=$2`, old.ID, fresh.ID)
	require.NoError(t, err)
	fresh.ID = old.ID
	require.NoError(t, p.local.MarkDelivered(ctx, site.DeliveryResponse{}))
	require.ErrorContains(t, p.local.MarkDelivered(ctx, r), "payload mismatch")
	snap := p.sync(t)
	require.Empty(t, snap.Acknowledged)
	p.balance(t, "99.00000000", "98.00000000")
	good := p.bill(t)
	r, err = p.primary.Deliver(ctx, p.peer, []site.Event{fresh, good})
	require.NoError(t, err)
	require.Equal(t, []int64{good.ID}, r.Acknowledged)
	require.Len(t, r.Rejected, 1)
	require.Contains(t, r.Rejected[0].Reason, "identity conflict")
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	p.sync(t)
	p.balance(t, "98.00000000", "97.00000000")
	var conflicts int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_event_conflicts WHERE node_id=$1`, p.peer.ID).Scan(&conflicts))
	require.Equal(t, 1, conflicts, "preserve conflicting evidence without replacing original receipt")
}

func TestSiteRejectedIdentityCollisionDoesNotRollbackOtherBills(t *testing.T) {
	p := newSiteFollowupPair(t)
	good, conflict := p.bill(t), p.bill(t)
	_, err := p.db.Exec(`INSERT INTO site_rejected_events(node_id,event_id,kind,payload,reason) VALUES($1,$2,'billing','{}','original')`, p.peer.ID, conflict.ID)
	require.NoError(t, err)
	r, err := p.primary.Deliver(context.Background(), p.peer, []site.Event{good, conflict})
	require.NoError(t, err)
	require.Equal(t, []int64{good.ID}, r.Acknowledged)
	require.Len(t, r.Rejected, 1)
	p.balance(t, "99.00000000", "98.00000000")
	var payload string
	require.NoError(t, p.db.QueryRow(`SELECT payload::text FROM site_rejected_events WHERE node_id=$1 AND event_id=$2`, p.peer.ID, conflict.ID).Scan(&payload))
	require.JSONEq(t, `{}`, payload)
}

func TestSiteBrokenDiagnosticDoesNotRollbackBillingAndRetriesAreBounded(t *testing.T) {
	p := newSiteFollowupPair(t)
	good := p.bill(t)
	delete(p.primary.logColumns, "ops_error_logs")
	diagnostic := site.Event{ID: 9000000, Kind: "ops_error_logs", Payload: json.RawMessage(`{"id":9000000,"request_id":"test-diagnostic"}`)}
	r, err := p.primary.Deliver(context.Background(), p.peer, []site.Event{diagnostic, good})
	require.NoError(t, err)
	require.Equal(t, []int64{good.ID}, r.Acknowledged)
	require.Len(t, r.Rejected, 1)
	require.True(t, r.Rejected[0].Retryable)
	p.balance(t, "99.00000000", "99.00000000")
	_, err = p.db.Exec(`UPDATE site_rejected_events SET attempts=20 WHERE node_id=$1 AND event_id=$2`, p.peer.ID, diagnostic.ID)
	require.NoError(t, err)
	r, err = p.primary.Deliver(context.Background(), p.peer, []site.Event{diagnostic, good})
	require.NoError(t, err)
	require.False(t, r.Rejected[0].Retryable)
	var state string
	require.NoError(t, p.db.QueryRow(`SELECT state FROM site_rejected_events WHERE node_id=$1 AND event_id=$2`, p.peer.ID, diagnostic.ID).Scan(&state))
	require.Equal(t, "pending", state)
	p.balance(t, "99.00000000", "99.00000000")
}

func TestSiteSettlementsRetireOnlyAfterReplicaConfirmsVersion(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	e := p.bill(t)
	_, err := p.db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, p.account)
	require.NoError(t, err)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	_, err = p.db.Exec(`UPDATE accounts SET deleted_at=NULL WHERE id=$1`, p.account)
	require.NoError(t, err)
	_, err = p.primary.Deliver(ctx, p.peer, []site.Event{e}) // Lose the success response.
	require.NoError(t, err)
	snap := p.sync(t)
	var found bool
	for _, row := range snap.Rows {
		found = found || row.Table == "$settlements" && len(row.Value) > 0
	}
	require.True(t, found)
	p.balance(t, "99.00000000", "99.00000000")
	var confirmed bool
	require.NoError(t, p.db.QueryRow(`SELECT confirmed_at IS NOT NULL FROM site_rejected_events WHERE node_id=$1 AND event_id=$2`, p.peer.ID, e.ID).Scan(&confirmed))
	require.False(t, confirmed)
	p.sync(t)
	require.NoError(t, p.db.QueryRow(`SELECT confirmed_at IS NOT NULL FROM site_rejected_events WHERE node_id=$1 AND event_id=$2`, p.peer.ID, e.ID).Scan(&confirmed))
	require.True(t, confirmed)
	_, err = p.db.Exec(`UPDATE site_rejected_events SET confirmed_at=NOW()-INTERVAL '91 days' WHERE node_id=$1 AND event_id=$2`, p.peer.ID, e.ID)
	require.NoError(t, err)
	p.sync(t)
	var remaining int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_rejected_events WHERE node_id=$1 AND event_id=$2`, p.peer.ID, e.ID).Scan(&remaining))
	require.Zero(t, remaining)
	r, err = p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.Equal(t, []int64{e.ID}, r.Acknowledged)
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
}
