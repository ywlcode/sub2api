//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSiteDeliveryPoisonLostACKAndCacheOutage(t *testing.T) {
	ctx := context.Background()
	db, client := newBillingAuditDatabase(t)
	replica, _ := newBillingAuditDatabase(t)
	group := mustCreateGroup(t, client, &service.Group{Name: "local", Platform: service.PlatformDeepseek, Status: service.StatusActive})
	user := mustCreateUser(t, client, &service.User{Email: "delivery@example.invalid", PasswordHash: "unused", Balance: 100})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, GroupID: &group.ID, Name: "local", Key: uuid.NewString()})
	account := mustCreateAccount(t, client, &service.Account{Name: "local", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	_, err := db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	peer := site.Peer{ID: "office", Token: strings.Repeat("x", 32)}
	primary, err := NewSiteStore(db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	local, err := NewSiteStore(replica, &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}})
	require.NoError(t, err)
	version := ""
	sync := func() site.Snapshot {
		ids, e := local.PendingIDs(ctx)
		require.NoError(t, e)
		snap, e := primary.ExportPage(ctx, peer, site.SyncRequest{Version: version, PendingIDs: ids})
		require.NoError(t, e)
		require.Empty(t, snap.Cursor)
		require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: snap.Full, BaseVersion: version, Version: snap.Version, Rows: snap.Rows, Acknowledged: snap.Acknowledged, Rejected: snap.Rejected}))
		version = snap.Version
		return snap
	}
	sync()
	invalidations := 0
	local.AfterInstall = func(context.Context) error { invalidations++; return nil }
	repo := &usageBillingRepository{db: replica, secondary: true}
	for i := 0; i < 3; i++ {
		cmd := service.UsageBillingCommand{RequestID: uuid.NewString(), UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: "deepseek-chat", BalanceCost: 1, AccountQuotaCost: 0.5}
		usage := service.UsageLog{RequestID: cmd.RequestID, UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: cmd.Model, CreatedAt: time.Now(), TotalCost: 1, ActualCost: 1}
		require.NoError(t, repo.StageUsageBilling(ctx, &cmd, &usage))
		_, err = repo.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: key.ID})
		require.NoError(t, err)
	}
	// Invalid middle bill is durable and isolated; other bills still settle.
	_, err = replica.Exec(`UPDATE site_outbox SET payload=jsonb_set(payload,'{version}','0'::jsonb) WHERE id=(SELECT id FROM site_outbox WHERE kind='billing' ORDER BY id OFFSET 1 LIMIT 1)`)
	require.NoError(t, err)
	events, err := local.Pending(ctx, site.DeliveryBatch)
	require.NoError(t, err)
	require.Len(t, events, 3)
	primary.AfterImport = func(context.Context, []service.UsageBillingEvent) error {
		return errors.New("Redis intentionally unavailable")
	}
	receipt, err := primary.Deliver(ctx, peer, events)
	require.NoError(t, err)
	require.Len(t, receipt.Acknowledged, 2)
	require.Len(t, receipt.Rejected, 1)
	retry, err := primary.Deliver(ctx, peer, events)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	// Simulate total loss of both upload responses. Pending IDs were durably
	// marked before the send; policy refresh must reconcile independently.
	sync()
	require.Zero(t, invalidations, "monetary reconciliation must not flush all request configuration")
	var balance string
	require.NoError(t, db.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "98.00000000", balance)
	require.NoError(t, replica.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "97.00000000", balance, "rejected bill remains reserved for reconciliation")
	var n int
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM site_outbox`).Scan(&n))
	require.Zero(t, n)
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM site_rejected_events`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_events WHERE state='applied'`).Scan(&n))
	require.Equal(t, 2, n, "durable cache compensation remains pending")
	// Missing historical subscriptions and accounts receive independent rejection
	// receipts; neither poisons other events or loses the original evidence.
	var missing service.UsageBillingEvent
	require.NoError(t, json.Unmarshal(events[0].Payload, &missing))
	missing.Command.RequestID = "missing-subscription"
	missing.Usage.RequestID = missing.Command.RequestID
	subID := int64(900000001)
	missing.Command.SubscriptionID = &subID
	missing.Usage.GroupID = &group.ID
	raw, e := json.Marshal(missing)
	require.NoError(t, e)
	rejected, e := primary.Deliver(ctx, peer, []site.Event{{ID: 900000001, Kind: "billing", Payload: raw}, events[0]})
	require.NoError(t, e)
	require.Len(t, rejected.Rejected, 1)
	require.Contains(t, rejected.Rejected[0].Reason, "subscription owner")
	require.Equal(t, []int64{events[0].ID}, rejected.Acknowledged)
	missing.Command.SubscriptionID = nil
	missing.Command.RequestID = "missing-account"
	missing.Usage.RequestID = missing.Command.RequestID
	missing.Command.AccountID = 900000002
	missing.Usage.AccountID = 900000002
	_, err = db.Exec(`INSERT INTO site_account_grants(node_id,account_id) VALUES($1,900000002)`, peer.ID)
	require.NoError(t, err)
	raw, e = json.Marshal(missing)
	require.NoError(t, e)
	rejected, e = primary.Deliver(ctx, peer, []site.Event{{ID: 900000002, Kind: "billing", Payload: raw}, events[0]})
	require.NoError(t, e)
	require.Len(t, rejected.Rejected, 1)
	require.Equal(t, []int64{events[0].ID}, rejected.Acknowledged)
	// Unrelated policy change cannot repeatedly debit a quarantined bill.
	_, err = db.Exec(`UPDATE users SET concurrency=17 WHERE id=$1`, user.ID)
	require.NoError(t, err)
	sync()
	sync()
	require.NoError(t, replica.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "97.00000000", balance)
	// An already accepted receipt survives removal of historical ownership.
	_, err = db.Exec(`UPDATE api_keys SET user_id=(SELECT min(id) FROM users) WHERE id=$1`, key.ID)
	require.NoError(t, err)
	_, err = primary.Deliver(ctx, peer, events)
	require.NoError(t, err)
	// Anonymous trace storms are local-only.
	_, err = replica.Exec(`INSERT INTO request_connection_traces(trace_id,started_at,request_id,client_request_id,user_id,api_key_id,path,stream,detail) SELECT md5(i::text)::uuid,NOW(),'anonymous-'||i,'',0,0,'/v1/responses',false,'{}'::jsonb FROM generate_series(1,2000) i`)
	require.NoError(t, err)
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM site_outbox`).Scan(&n))
	require.Zero(t, n)
	// Large diagnostics cannot inflate the control request or starve billing.
	_, err = replica.Exec(`INSERT INTO site_outbox(kind,payload) SELECT 'ops_error_logs','{}'::jsonb FROM generate_series(1,1000100)`)
	require.NoError(t, err)
	ids, err := local.PendingIDs(ctx)
	require.NoError(t, err)
	require.Empty(t, ids)
	page, err := local.Pending(ctx, site.DeliveryBatch)
	require.NoError(t, err)
	require.Len(t, page, site.DeliveryBatch)
	// Drop only these artificial backlog fixtures, then exercise real throughput.
	_, err = replica.Exec(`DELETE FROM site_outbox`)
	require.NoError(t, err)
	_, err = replica.Exec(`INSERT INTO ops_error_logs(request_id,api_key_id,user_id,account_id,error_phase,error_type,error_message) SELECT 'load-'||i,$1,$2,$3,'upstream','timeout','load-test' FROM generate_series(1,1600) i`, key.ID, user.ID, account.ID)
	require.NoError(t, err)
	started := time.Now()
	batches := 0
	for {
		events, e := local.Pending(ctx, site.DeliveryBatch)
		require.NoError(t, e)
		if len(events) == 0 {
			break
		}
		receipt, e := primary.Deliver(ctx, peer, events)
		require.NoError(t, e)
		require.Empty(t, receipt.Rejected)
		require.NoError(t, local.MarkDelivered(ctx, receipt))
		batches++
	}
	require.Greater(t, batches, 1)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ops_error_logs WHERE error_message='load-test'`).Scan(&n))
	require.Equal(t, 1600, n)
	t.Logf("1600 diagnostics drained in %d immediate batches, %s; million-entry backlog had zero control IDs", batches, time.Since(started))

}

func TestSiteProjectionPagingScopeAndRowDelta(t *testing.T) {
	ctx := context.Background()
	db, client := newBillingAuditDatabase(t)
	group := mustCreateGroup(t, client, &service.Group{Name: "local", Platform: service.PlatformDeepseek, Status: service.StatusActive})
	foreign := mustCreateGroup(t, client, &service.Group{Name: "foreign", Platform: service.PlatformOpenAI, Status: service.StatusActive})
	account := mustCreateAccount(t, client, &service.Account{Name: "local", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	_, err := db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	// Model mappings inflate a real table enough to exceed the former 32 MiB
	// single-response ceiling. Foreign-only owners/keys must never be projected.
	_, err = db.Exec(`INSERT INTO users(email,password_hash,balance) SELECT 'scale-'||i||'@invalid','unused',100 FROM generate_series(1,5000) i`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO api_keys(user_id,key,name,group_id,model_mapping) SELECT id,'scale-'||id,'scale',CASE WHEN id%2=0 THEN $1::bigint ELSE $2::bigint END,jsonb_build_object('alias',repeat('m',14000)) FROM users`, group.ID, foreign.ID)
	require.NoError(t, err)
	primary, err := NewSiteStore(db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	peer := site.Peer{ID: "office", Token: strings.Repeat("x", 32)}
	request := site.SyncRequest{}
	var version string
	var all []site.Row
	count, size, pages := 0, 0, 0
	started := time.Now()
	for {
		snap, e := primary.ExportPage(ctx, peer, request)
		require.NoError(t, e)
		require.True(t, snap.Full)
		raw, e := json.Marshal(snap.Rows)
		require.NoError(t, e)
		require.Less(t, len(raw), 32<<20)
		size += len(raw)
		all = append(all, snap.Rows...)
		pages++
		for _, row := range snap.Rows {
			if row.Table == "api_keys" {
				count++
				require.NotContains(t, string(row.Value), `"group_id":`+jsonNumber(foreign.ID)+",")
			}
		}
		version = snap.Version
		if snap.Cursor == "" {
			break
		}
		request.TargetVersion, request.Cursor = snap.Version, snap.Cursor
	}
	require.Equal(t, 2500, count)
	require.Greater(t, size, 32<<20)
	require.Greater(t, pages, 1)
	t.Logf("bootstrap %d pages, %d JSON bytes, %s", pages, size, time.Since(started))
	replica, _ := newBillingAuditDatabase(t)
	local, err := NewSiteStore(replica, &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}})
	require.NoError(t, err)
	require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: true, Version: version, Rows: all}))
	var keyCount int
	require.NoError(t, replica.QueryRow(`SELECT count(*) FROM api_keys`).Scan(&keyCount))
	require.Equal(t, 2500, keyCount)
	var untouched string
	require.NoError(t, replica.QueryRow(`SELECT xmin::text FROM users WHERE id=4`).Scan(&untouched))

	primary2, err := NewSiteStore(db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	snap, err := primary2.ExportPage(ctx, peer, site.SyncRequest{Version: version})
	require.NoError(t, err)
	require.False(t, snap.Full)
	require.Empty(t, snap.Rows, "restart retains incremental cursor")
	_, err = db.Exec(`UPDATE users SET balance=99 WHERE id=2`)
	require.NoError(t, err)
	snap, err = primary2.ExportPage(ctx, peer, site.SyncRequest{Version: version})
	require.NoError(t, err)
	require.Len(t, snap.Rows, 1)
	require.Equal(t, "users", snap.Rows[0].Table)
	require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "main", BaseVersion: version, Version: snap.Version, Rows: snap.Rows}))
	var after string
	require.NoError(t, replica.QueryRow(`SELECT xmin::text FROM users WHERE id=4`).Scan(&after))
	require.Equal(t, untouched, after, "delta must not rewrite unrelated users")
	version = snap.Version
	_, err = db.Exec(`UPDATE api_keys SET group_id=$1 WHERE user_id=2`, foreign.ID)
	require.NoError(t, err)
	snap, err = primary2.ExportPage(ctx, peer, site.SyncRequest{Version: version})
	require.NoError(t, err)
	require.Len(t, snap.Rows, 2, "revoked key and owner only, no unrelated credentials")
	for _, row := range snap.Rows {
		require.Empty(t, row.Value)
	}
}
