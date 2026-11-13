//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type siteFollowupPair struct {
	primary, local     *SiteStore
	db, replica        *sql.DB
	peer               site.Peer
	user, key, account int64
	version            string
}

func newSiteFollowupPair(t *testing.T) *siteFollowupPair {
	t.Helper()
	db, client := newBillingAuditDatabase(t)
	replica, _ := newBillingAuditDatabase(t)
	g := mustCreateGroup(t, client, &service.Group{Name: "local", Platform: service.PlatformDeepseek, Status: service.StatusActive})
	u := mustCreateUser(t, client, &service.User{Email: "followup@example.invalid", PasswordHash: "unused", Balance: 100})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, GroupID: &g.ID, Name: "local", Key: uuid.NewString()})
	a := mustCreateAccount(t, client, &service.Account{Name: "local", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	_, err := db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, a.ID, g.ID)
	require.NoError(t, err)
	p := &siteFollowupPair{db: db, replica: replica, peer: site.Peer{ID: "office", Token: strings.Repeat("x", 32)}, user: u.ID, key: k.ID, account: a.ID}
	p.primary, err = NewSiteStore(db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	p.local, err = NewSiteStore(replica, &config.Config{Site: site.Config{Mode: site.Secondary, ID: p.peer.ID, Token: p.peer.Token}})
	require.NoError(t, err)
	p.sync(t)
	return p
}

func (p *siteFollowupPair) sync(t *testing.T) site.Snapshot {
	t.Helper()
	ctx := context.Background()
	ids, err := p.local.PendingIDs(ctx)
	require.NoError(t, err)
	snap, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: p.version, PendingIDs: ids})
	require.NoError(t, err)
	require.Empty(t, snap.Cursor)
	require.NoError(t, p.local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: snap.Full, BaseVersion: p.version, Version: snap.Version, Rows: snap.Rows, Acknowledged: snap.Acknowledged, Rejected: snap.Rejected}))
	p.version = snap.Version
	return snap
}

func (p *siteFollowupPair) balance(t *testing.T, primary, local string) {
	t.Helper()
	for i, db := range []*sql.DB{p.db, p.replica} {
		var got string
		require.NoError(t, db.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, p.user).Scan(&got))
		require.Equal(t, []string{primary, local}[i], got)
	}
}

func (p *siteFollowupPair) bill(t *testing.T) site.Event {
	t.Helper()
	ctx := context.Background()
	r := &usageBillingRepository{db: p.replica, secondary: true}
	c := service.UsageBillingCommand{RequestID: uuid.NewString(), UserID: p.user, APIKeyID: p.key, AccountID: p.account, Model: "deepseek-chat", BalanceCost: 1, AccountQuotaCost: 0.5}
	u := service.UsageLog{RequestID: c.RequestID, UserID: p.user, APIKeyID: p.key, AccountID: p.account, Model: c.Model, CreatedAt: time.Now(), TotalCost: 1, ActualCost: 1}
	require.NoError(t, r.StageUsageBilling(ctx, &c, &u))
	_, err := r.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: c.RequestID, APIKeyID: c.APIKeyID})
	require.NoError(t, err)
	events, err := p.local.Pending(ctx, site.DeliveryBatch)
	require.NoError(t, err)
	for _, e := range events {
		if e.Kind == "billing" && strings.Contains(string(e.Payload), c.RequestID) {
			return e
		}
	}
	t.Fatal("bill missing from outbox")
	return site.Event{}
}

func TestSiteProjectionDoesNotBlockBillingAndPreservesLateChanges(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	_, err := p.db.Exec(`UPDATE users SET balance=99 WHERE id=$1`, p.user)
	require.NoError(t, err)
	export := p.primary.ExportRequestPolicy
	p.primary.ExportRequestPolicy = func() ([]byte, error) {
		writeCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		_, e := p.db.ExecContext(writeCtx, `UPDATE users SET balance=98 WHERE id=$1`, p.user)
		if e != nil {
			return nil, fmt.Errorf("snapshot blocked live billing: %w", e)
		}
		return export()
	}
	p.sync(t)
	p.balance(t, "98.00000000", "99.00000000")
	p.primary.ExportRequestPolicy = export
	p.sync(t)
	p.balance(t, "98.00000000", "98.00000000")
	// A writer uncommitted at cleanup is skipped, not waited on or forgotten.
	_, err = p.db.Exec(`UPDATE users SET balance=97 WHERE id=$1`, p.user)
	require.NoError(t, err)
	var writer *sql.Tx
	p.primary.ExportRequestPolicy = func() ([]byte, error) {
		var e error
		writer, e = p.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		_, e = writer.ExecContext(ctx, `UPDATE users SET balance=96 WHERE id=$1`, p.user)
		if e != nil {
			return nil, e
		}
		return export()
	}
	defer func() {
		if writer != nil {
			_ = writer.Rollback()
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	snap, err := p.primary.ExportPage(readCtx, p.peer, site.SyncRequest{Version: p.version})
	cancel()
	require.NoError(t, err)
	require.NotEmpty(t, snap.Rows)
	require.NoError(t, writer.Commit())
	p.primary.ExportRequestPolicy = export
	p.sync(t)
	p.balance(t, "96.00000000", "96.00000000")
	// A failed publication cannot consume the queue.
	_, err = p.db.Exec(`UPDATE users SET balance=95 WHERE id=$1`, p.user)
	require.NoError(t, err)
	p.primary.ExportRequestPolicy = func() ([]byte, error) { return nil, errors.New("injected export failure") }
	_, err = p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Version: p.version})
	require.Error(t, err)
	p.primary.ExportRequestPolicy = export
	p.sync(t)
	p.balance(t, "95.00000000", "95.00000000")
}

func TestSiteConcurrentExportersAndHotBalance(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	other, err := NewSiteStore(p.db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 40; j++ {
				if _, e := p.db.ExecContext(ctx, `UPDATE users SET balance=balance-0.01 WHERE id=$1`, p.user); e != nil {
					errs <- e
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			version := p.version
			for j := 0; j < 20; j++ {
				snap, e := []*SiteStore{p.primary, other}[n%2].ExportPage(ctx, p.peer, site.SyncRequest{Version: version})
				if e != nil {
					errs <- e
					return
				}
				version = snap.Version
			}
		}(i)
	}
	started := time.Now()
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	p.sync(t)
	p.balance(t, "93.60000000", "93.60000000")
	t.Logf("640 contended debits and 80 exports through two instances: %s", time.Since(started))
}

func TestSiteReconciliationManualSettlementAndWaiver(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	e := p.bill(t)
	var bad service.UsageBillingEvent
	require.NoError(t, json.Unmarshal(e.Payload, &bad))
	bad.Command.Model = "" // structural failure, not a temporary missing dependency
	bad.Version = 0
	raw, err := json.Marshal(bad)
	require.NoError(t, err)
	e.Payload = raw
	_, err = p.replica.Exec(`UPDATE site_outbox SET payload=$2::jsonb WHERE id=$1`, e.ID, string(raw))
	require.NoError(t, err)
	result, err := p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.Len(t, result.Rejected, 1)
	require.False(t, result.Rejected[0].Retryable)
	require.NoError(t, p.local.MarkDelivered(ctx, result))
	p.sync(t)
	p.balance(t, "100.00000000", "99.00000000")
	// Independent manual charge happens first; until reconciliation the local
	// reservation is still intentionally retained, then removed exactly once.
	_, err = p.db.Exec(`UPDATE users SET balance=balance-1 WHERE id=$1`, p.user)
	require.NoError(t, err)
	p.sync(t)
	p.balance(t, "99.00000000", "98.00000000")
	r, err := p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "settled", "Manual debit verified", 1)
	require.NoError(t, err)
	require.Equal(t, "settled", r.State)
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
	_, err = p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "settled", "Repeated button click", 1)
	require.NoError(t, err)
	_, err = p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "waived", "Conflicting resolution", 1)
	require.ErrorIs(t, err, service.ErrSiteReconciliationConflict)
	// A stale rejected delivery response cannot resurrect the reservation.
	require.NoError(t, p.local.MarkDelivered(ctx, result))
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
	row, err := p.local.GetSiteRejection(ctx, p.peer.ID, e.ID)
	require.NoError(t, err)
	require.Equal(t, "settled", row.State)
	require.JSONEq(t, string(raw), string(row.Payload))
	// A deferred bill can also be waived without charging the primary.
	e = p.bill(t)
	require.NoError(t, json.Unmarshal(e.Payload, &bad))
	bad.Usage.CreatedAt = time.Now().Add(time.Hour)
	e.Payload, err = json.Marshal(bad)
	require.NoError(t, err)
	_, err = p.replica.Exec(`UPDATE site_outbox SET payload=$2::jsonb WHERE id=$1`, e.ID, string(e.Payload))
	require.NoError(t, err)
	result, err = p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.True(t, result.Rejected[0].Retryable)
	require.NoError(t, p.local.MarkDelivered(ctx, result))
	_, err = p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "waived", "No charge approved", 1)
	require.NoError(t, err)
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
}

func TestSiteDeferredClockBillRetriesWithoutBlockingOthers(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	deferred := p.bill(t)
	good := p.bill(t)
	var future service.UsageBillingEvent
	require.NoError(t, json.Unmarshal(deferred.Payload, &future))
	retryAt := time.Now().Add(1500 * time.Millisecond)
	future.Usage.CreatedAt = retryAt.Add(time.Minute)
	var err error
	deferred.Payload, err = json.Marshal(future)
	require.NoError(t, err)
	_, err = p.replica.Exec(`UPDATE site_outbox SET payload=$2::jsonb WHERE id=$1`, deferred.ID, string(deferred.Payload))
	require.NoError(t, err)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{deferred, good})
	require.NoError(t, err)
	require.Equal(t, []int64{good.ID}, r.Acknowledged)
	require.Len(t, r.Rejected, 1)
	require.True(t, r.Rejected[0].Retryable)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	p.sync(t)
	p.balance(t, "99.00000000", "98.00000000")
	ready, err := p.local.Pending(ctx, site.DeliveryBatch)
	require.NoError(t, err)
	require.Empty(t, ready, "deferred event must respect retry delay")
	ids, err := p.local.PendingIDs(ctx)
	require.NoError(t, err)
	require.Empty(t, ids, "deferred events must not fill the receipt window")
	time.Sleep(max(0, time.Until(retryAt)))
	r, err = p.primary.Deliver(ctx, p.peer, []site.Event{deferred})
	require.NoError(t, err)
	require.Empty(t, r.Rejected)
	// Lose the successful upload response: the settlement row must release a
	// deferred reservation even though its attempted flag was reset for backoff.
	p.sync(t)
	p.balance(t, "98.00000000", "98.00000000")
	require.NoError(t, p.local.MarkDelivered(ctx, site.DeliveryResponse{Rejected: []site.Rejection{{ID: deferred.ID, Reason: "late retry notice", Retryable: true}}}))
	p.sync(t)
	p.balance(t, "98.00000000", "98.00000000")
	row, err := p.local.GetSiteRejection(ctx, p.peer.ID, deferred.ID)
	require.NoError(t, err)
	require.Equal(t, "settled", row.State)
}

func TestSiteReconciliationRetriesLegacyRejectedBill(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	e := p.bill(t)
	// Reproduce a pre-fix permanent rejection after its dependency was repaired.
	_, err := p.db.Exec(`INSERT INTO site_rejected_events(node_id,event_id,kind,payload,reason) VALUES($1,$2,$3,$4::jsonb,'legacy missing account')`, p.peer.ID, e.ID, e.Kind, string(e.Payload))
	require.NoError(t, err)
	_, err = p.db.Exec(`INSERT INTO site_receipts(node_id,event_id,digest,rejection) VALUES($1,$2,'original-wire-digest','legacy missing account')`, p.peer.ID, e.ID)
	require.NoError(t, err)
	require.NoError(t, p.local.MarkDelivered(ctx, site.DeliveryResponse{Rejected: []site.Rejection{{ID: e.ID, Reason: "legacy missing account"}}}))
	_, err = p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "retry", "Dependency repaired; retry original bill", 1)
	require.NoError(t, err)
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
	_, err = p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "retry", "Duplicate admin retry", 1)
	require.NoError(t, err)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.Equal(t, []int64{e.ID}, r.Acknowledged)
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
}

func TestSiteUploadDuringSnapshotKeepsReceiptsAndDebitsTogether(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	var events []site.Event
	for i := 0; i < 20; i++ {
		events = append(events, p.bill(t))
	}
	export := p.primary.ExportRequestPolicy
	p.primary.ExportRequestPolicy = func() ([]byte, error) {
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := p.primary.Deliver(writeCtx, p.peer, events)
		if err != nil {
			return nil, err
		}
		return export()
	}
	snap := p.sync(t)
	require.Empty(t, snap.Acknowledged, "uploads committed after snapshot start are confirmed next time")
	p.balance(t, "80.00000000", "80.00000000")
	p.primary.ExportRequestPolicy = export
	snap = p.sync(t)
	require.Len(t, snap.Acknowledged, 20)
	p.balance(t, "80.00000000", "80.00000000")
}

func TestSiteDeletedAccountRetriesAfterRepair(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	e := p.bill(t)
	_, err := p.db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, p.account)
	require.NoError(t, err)
	r, err := p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.Len(t, r.Rejected, 1)
	require.True(t, r.Rejected[0].Retryable)
	require.NoError(t, p.local.MarkDelivered(ctx, r))
	// Retrying before repair must roll back, preserving the reservation and the
	// original rejection for inspection rather than claiming it was settled.
	_, err = p.primary.ResolveSiteRejection(ctx, p.peer.ID, e.ID, "retry", "Dependency still missing", 1)
	require.ErrorIs(t, err, service.ErrSiteReconciliationBlocked)
	p.balance(t, "100.00000000", "99.00000000")
	_, err = p.db.Exec(`UPDATE accounts SET deleted_at=NULL WHERE id=$1`, p.account)
	require.NoError(t, err)
	r, err = p.primary.Deliver(ctx, p.peer, []site.Event{e})
	require.NoError(t, err)
	require.Empty(t, r.Rejected)
	p.sync(t)
	p.balance(t, "99.00000000", "99.00000000")
}
