//go:build integration

package repository

import (
	"context"
	"encoding/json"
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

func TestSiteReplicationBillingRetryPendingOverlayAndRevocation(t *testing.T) {
	ctx := context.Background()
	primaryDB, primaryClient := newBillingAuditDatabase(t)
	localDB, localClient := newBillingAuditDatabase(t)
	user := mustCreateUser(t, primaryClient, &service.User{Email: uuid.NewString() + "@example.com", PasswordHash: "never-export-this", Balance: 100})
	key := mustCreateApiKey(t, primaryClient, &service.APIKey{UserID: user.ID, Key: "sk-private-" + uuid.NewString(), Name: "office", Quota: 100})
	account := mustCreateAccount(t, primaryClient, &service.Account{Name: "domestic-" + uuid.NewString(), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	other := mustCreateAccount(t, primaryClient, &service.Account{Name: "foreign-" + uuid.NewString(), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey})
	proxy, err := primaryClient.Proxy.Create().SetName("primary-network-only").SetProtocol("socks5").SetHost("proxy.example.invalid").SetPort(1080).SetPassword("never-export-proxy-password").Save(ctx)
	require.NoError(t, err)
	_, err = primaryDB.Exec(`UPDATE accounts SET proxy_id=$1,proxy_fallback_origin_id=$1 WHERE id=$2`, proxy.ID, account.ID)
	require.NoError(t, err)
	peer := site.Peer{ID: "office", Token: strings.Repeat("s", 32)}
	_, err = primaryDB.Exec(`INSERT INTO security_penalty_subjects(user_id,scope,subject_key,policy,blocked_until) VALUES($1,'session','会话:with,separator','session_block',NOW()+INTERVAL '1 hour')`, user.ID)
	require.NoError(t, err)
	primary, err := NewSiteStore(primaryDB, &config.Config{Site: site.Config{Mode: site.Primary, ID: "bosc", Peers: []site.Peer{peer}}})
	require.NoError(t, err)
	local, err := NewSiteStore(localDB, &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}})
	require.NoError(t, err)
	require.Equal(t, primary.Schema(), local.Schema())
	snapshot, err := primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	for _, r := range snapshot.Rows {
		require.NotEqual(t, "proxies", r.Table)
		require.NotContains(t, string(r.Value), "never-export-proxy-password")
		require.NotContains(t, string(r.Value), "never-export-this")
		require.NotContains(t, string(r.Value), key.Key)
		if r.Table == "accounts" {
			require.NotEqual(t, r.Key, strings.TrimSpace(jsonNumber(other.ID)))
		}
	}
	response := site.SyncResponse{Protocol: site.ProtocolVersion, PrimaryID: "bosc", NodeID: peer.ID, Schema: primary.Schema(), Full: true, Version: "v1", Rows: snapshot.Rows}
	require.NoError(t, local.Install(ctx, response))
	var direct bool
	require.NoError(t, localDB.QueryRow(`SELECT proxy_id IS NULL AND proxy_fallback_origin_id IS NULL FROM accounts WHERE id=$1`, account.ID).Scan(&direct))
	require.True(t, direct, "secondary must use its own network")
	require.NoError(t, primaryDB.QueryRow(`SELECT proxy_id=$1 AND proxy_fallback_origin_id=$1 FROM accounts WHERE id=$2`, proxy.ID, account.ID).Scan(&direct))
	require.True(t, direct, "export must preserve the primary's proxy binding")
	var restricted int
	require.NoError(t, localDB.QueryRow(`SELECT count(*) FROM security_penalty_subjects`).Scan(&restricted))
	require.Zero(t, restricted)
	wrongPrimary := response
	wrongPrimary.PrimaryID = "ecos"
	require.ErrorContains(t, local.Install(ctx, wrongPrimary), "another primary")
	replicaKey, err := NewAPIKeyRepository(localClient, localDB).GetByKeyForAuth(ctx, site.KeyDigest(peer.Token, key.Key))
	require.NoError(t, err)
	require.Equal(t, key.ID, replicaKey.ID)
	_, err = NewAPIKeyRepository(localClient, localDB).GetByKeyForAuth(ctx, key.Key)
	require.Error(t, err)
	var count int
	require.NoError(t, localDB.QueryRow(`SELECT count(*) FROM accounts`).Scan(&count))
	require.Equal(t, 1, count)
	// Full refreshes and monetary overlays preserve local upstream cooldowns;
	// authoritative credential changes can deliberately clear that state.
	_, err = localDB.Exec(`UPDATE accounts SET status='error',schedulable=false,error_message='local 401',temp_unschedulable_until=NOW()+INTERVAL '10 minutes',extra='{"model_rate_limits":{"deepseek-v4":{"reset_at":"2099-01-01T00:00:00Z"}}}' WHERE id=$1`, account.ID)
	require.NoError(t, err)
	require.NoError(t, local.Install(ctx, response))
	var status string
	var blocked bool
	require.NoError(t, localDB.QueryRow(`SELECT status,temp_unschedulable_until>NOW() AND extra ? 'model_rate_limits' FROM accounts WHERE id=$1`, account.ID).Scan(&status, &blocked))
	require.Equal(t, "error", status)
	require.True(t, blocked)
	_, err = primaryDB.Exec(`UPDATE accounts SET credentials='{"api_key":"rotated-domestic-key"}',status='active',schedulable=true WHERE id=$1`, account.ID)
	require.NoError(t, err)
	snapshot, err = primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	response.Rows = snapshot.Rows
	require.NoError(t, local.Install(ctx, response))
	require.NoError(t, localDB.QueryRow(`SELECT status,temp_unschedulable_until IS NULL AND NOT(extra ? 'model_rate_limits') FROM accounts WHERE id=$1`, account.ID).Scan(&status, &blocked))
	require.Equal(t, "active", status)
	require.True(t, blocked)
	// A no-change diff must not write policy rows or manufacture moderation
	// status revisions, including when monetary rows are reapplied later.
	var revision int64
	require.NoError(t, localDB.QueryRow(`SELECT moderation_status_revision FROM users WHERE id=$1`, user.ID).Scan(&revision))
	require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "bosc", BaseVersion: "v1", Version: "v1"}))

	billing := &usageBillingRepository{db: localDB, secondary: true}
	cmd := service.UsageBillingCommand{RequestID: uuid.NewString(), UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: "deepseek-v4", BalanceCost: 1.23456789, APIKeyQuotaCost: 1.23456789, APIKeyRateLimitCost: 1.23456789}
	usage := service.UsageLog{RequestID: cmd.RequestID, UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: cmd.Model, ActualCost: cmd.BalanceCost, TotalCost: cmd.BalanceCost, CreatedAt: time.Now().UTC(), InputTokens: 100, OutputTokens: 20}
	require.NoError(t, billing.StageUsageBilling(ctx, &cmd, &usage))
	_, err = billing.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: key.ID})
	require.NoError(t, err)
	var balance string
	require.NoError(t, localDB.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "98.76543211", balance)
	var afterRevision int64
	require.NoError(t, localDB.QueryRow(`SELECT moderation_status_revision FROM users WHERE id=$1`, user.ID).Scan(&afterRevision))
	require.Equal(t, revision, afterRevision)
	pending, err := local.Pending(ctx, 128)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	// A successful heartbeat without an upload must not erase pending spending.
	require.NoError(t, local.Install(ctx, response))
	require.NoError(t, localDB.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "98.76543211", balance)

	snapshot, err = primary.Export(ctx, peer, pending, []int64{pending[0].ID})
	require.NoError(t, err)
	// Simulate a lost network response: the primary retries exactly the same bill.
	_, err = primary.Export(ctx, peer, pending, []int64{pending[0].ID})
	require.NoError(t, err)
	require.NoError(t, primaryDB.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "98.76543211", balance)
	require.NoError(t, primaryDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE user_id=$1`, user.ID).Scan(&count))
	require.Equal(t, 1, count)
	response.Version = "v2"
	response.Rows = snapshot.Rows
	response.Acknowledged = []int64{pending[0].ID}
	require.NoError(t, local.Install(ctx, response))
	require.NoError(t, localDB.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "98.76543211", balance, "ack must not charge a second time")
	pending, err = local.Pending(ctx, 128)
	require.NoError(t, err)
	require.Empty(t, pending)
	_, err = primaryDB.Exec(`UPDATE api_keys SET status='disabled' WHERE id=$1`, key.ID)
	require.NoError(t, err)
	snapshot, err = primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	response.Rows = snapshot.Rows
	response.Version = "v3"
	response.Acknowledged = nil
	require.NoError(t, local.Install(ctx, response))
	replicaKey, err = NewAPIKeyRepository(localClient, localDB).GetByKeyForAuth(ctx, site.KeyDigest(peer.Token, key.Key))
	require.NoError(t, err)
	require.False(t, replicaKey.IsActive())
	// An unrelated production database is never silently overwritten as a replica.
	_, err = NewSiteStore(primaryDB, &config.Config{Site: site.Config{Mode: site.Secondary, ID: "accidental"}})
	require.Error(t, err)
	// Diagnostic errors retain recovered state updates and never create a new
	// error entry just because delivery/retry happened twice.
	var errorID int64
	require.NoError(t, localDB.QueryRow(`INSERT INTO ops_error_logs(request_id,account_id,error_phase,error_type,error_message,status_code) VALUES('trace-one',$1,'upstream','timeout','initial timeout',408) RETURNING id`, account.ID).Scan(&errorID))
	_, err = localDB.Exec(`UPDATE ops_error_logs SET error_message='recovered by retry' WHERE id=$1`, errorID)
	require.NoError(t, err)
	pending, err = local.Pending(ctx, 128)
	require.NoError(t, err)
	ids, err := local.PendingIDs(ctx)
	require.NoError(t, err)
	_, err = primary.Export(ctx, peer, pending, ids)
	require.NoError(t, err)
	_, err = primary.Export(ctx, peer, pending, ids)
	require.NoError(t, err)
	var message string
	require.NoError(t, primaryDB.QueryRow(`SELECT error_message FROM ops_error_logs WHERE request_id=$1`, siteRequestID(peer.ID, "trace-one")).Scan(&message))
	require.Equal(t, "recovered by retry", message)
	// Confirm all pending receipts even when an already accepted bill/log is
	// outside this upload page; sequence allocation is not commit ordering.
	snapshot, err = primary.Export(ctx, peer, nil, ids)
	require.NoError(t, err)
	require.ElementsMatch(t, ids, snapshot.Acknowledged)
	response.Version = "v4"
	response.Rows = snapshot.Rows
	response.Acknowledged = snapshot.Acknowledged
	require.NoError(t, local.Install(ctx, response))
	// Simultaneous local settlement and snapshot refresh cannot lose a debit.
	cmd.RequestID = uuid.NewString()
	cmd.RequestFingerprint = ""
	usage.RequestID = cmd.RequestID
	require.NoError(t, billing.StageUsageBilling(ctx, &cmd, &usage))
	response.Acknowledged = nil
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- local.Install(ctx, response) }()
	go func() {
		defer wg.Done()
		_, e := billing.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: key.ID})
		errs <- e
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	require.NoError(t, localDB.QueryRow(`SELECT balance::text FROM users WHERE id=$1`, user.ID).Scan(&balance))
	require.Equal(t, "97.53086422", balance)
	require.NoError(t, billing.RecordSiteUsageIssue(ctx, service.SiteUsageIssue{RequestID: "needs-review", UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: "deepseek-v4", Reason: "upstream_usage_unavailable"}))
	pending, err = local.Pending(ctx, 128)
	require.NoError(t, err)
	ids, err = local.PendingIDs(ctx)
	require.NoError(t, err)
	_, err = primary.Export(ctx, peer, pending, ids)
	require.NoError(t, err)
	require.NoError(t, primaryDB.QueryRow(`SELECT count(*) FROM ops_error_logs WHERE request_id=$1 AND error_type='site_usage_unsettled' AND status_code IS NULL`, siteRequestID(peer.ID, "needs-review")).Scan(&count))
	require.Equal(t, 1, count, "unsettled bill must persist and upload without inventing a client failure")
	require.NoError(t, primaryDB.QueryRow(`SELECT count(*) FROM usage_logs WHERE request_id=$1`, siteRequestID(peer.ID, "needs-review")).Scan(&count))
	require.Zero(t, count, "missing usage must not create a free bill")
	traceID := uuid.NewString()
	_, err = localDB.Exec(`INSERT INTO request_connection_traces(trace_id,started_at,request_id,client_request_id,user_id,api_key_id,account_id,path,stream,detail) VALUES($1,NOW(),'needs-review','original-client-id',$2,$3,$4,'/v1/responses',true,'{"http_status":200,"stream_failed":true,"upload_ms":1234}')`, traceID, user.ID, key.ID, account.ID)
	require.NoError(t, err)
	pending, err = local.Pending(ctx, 128)
	require.NoError(t, err)
	ids, err = local.PendingIDs(ctx)
	require.NoError(t, err)
	snapshot, err = primary.Export(ctx, peer, pending, ids)
	require.NoError(t, err)
	var source, node, clientID string
	var upload int
	require.NoError(t, primaryDB.QueryRow(`SELECT source,detail->>'site_node_id',client_request_id,(detail->>'upload_ms')::int FROM request_connection_traces WHERE request_id=$1`, siteRequestID(peer.ID, "needs-review")).Scan(&source, &node, &clientID, &upload))
	require.Equal(t, "application", source)
	require.Equal(t, peer.ID, node)
	require.Equal(t, "original-client-id", clientID)
	require.Equal(t, 1234, upload)
	response.Rows, response.Acknowledged = snapshot.Rows, snapshot.Acknowledged
	require.NoError(t, local.Install(ctx, response))
	// Revoke an account with one final in-flight bill still pending. Subsequent
	// heartbeats must not repeatedly add that debit to the retained FK row.
	cmd.RequestID, cmd.RequestFingerprint = uuid.NewString(), ""
	cmd.AccountQuotaCost = 2
	usage.RequestID = cmd.RequestID
	require.NoError(t, billing.StageUsageBilling(ctx, &cmd, &usage))
	_, err = billing.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: key.ID})
	require.NoError(t, err)
	_ = mustCreateAccount(t, primaryClient, &service.Account{Name: "replacement-" + uuid.NewString(), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	_, err = primaryDB.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{site_secondary_enabled}','false') WHERE id=$1`, account.ID)
	require.NoError(t, err)
	snapshot, err = primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	response.Rows, response.Acknowledged = snapshot.Rows, nil
	require.NoError(t, local.Install(ctx, response))
	require.NoError(t, local.Install(ctx, response))
	var accountUsed float64
	require.NoError(t, localDB.QueryRow(`SELECT (extra->>'quota_used')::numeric FROM accounts WHERE id=$1 AND deleted_at IS NOT NULL`, account.ID).Scan(&accountUsed))
	require.Equal(t, 2.0, accountUsed)
	pending, err = local.Pending(ctx, 128)
	require.NoError(t, err)
	ids, err = local.PendingIDs(ctx)
	require.NoError(t, err)
	_, err = primary.Export(ctx, peer, pending, ids)
	require.NoError(t, err, "previously granted accounts can settle pre-revocation calls")
	// Different peers share the primary ledger. Serialization conflicts while
	// charging the same user must be retried without duplicate/missing bills.
	var billingEvent site.Event
	for _, event := range pending {
		if event.Kind == "billing" {
			billingEvent = event
			break
		}
	}
	require.NotEmpty(t, billingEvent.Payload)
	errs = make(chan error, 2)
	for _, nodeID := range []string{"office-two", "office-three"} {
		p := peer
		p.ID = nodeID
		_, err = primaryDB.Exec(`INSERT INTO site_account_grants(node_id,account_id) VALUES($1,$2)`, p.ID, account.ID)
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := primary.Export(ctx, p, []site.Event{billingEvent}, []int64{billingEvent.ID})
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
}

func jsonNumber(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestSiteAccountSwitchChangesGrantAndRoutingWithoutRestart(t *testing.T) {
	ctx := context.Background()
	pdb, client := newBillingAuditDatabase(t)
	sdb, _ := newBillingAuditDatabase(t)
	peer := site.Peer{ID: "office", Token: strings.Repeat("t", 32)}
	group := mustCreateGroup(t, client, &service.Group{Name: "switch-group", Platform: service.PlatformComposite, Status: service.StatusActive})
	user := mustCreateUser(t, client, &service.User{Email: "switch@example.invalid", PasswordHash: "disabled", Balance: 100})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, GroupID: &group.ID, Name: "test", Key: "sk-switch-test", Status: service.StatusActive})
	account := mustCreateAccount(t, client, &service.Account{Name: "shared", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "test-secret", "model_mapping": map[string]any{"deepseek-custom-*": "deepseek-flash"}}, Extra: map[string]any{"unrelated": true}})
	_, err := pdb.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	primary, err := NewSiteStore(pdb, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	secondary, err := NewSiteStore(sdb, &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}})
	require.NoError(t, err)
	for _, enabled := range []bool{false, true, false, true} {
		_, err = pdb.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{site_secondary_enabled}',to_jsonb($2::boolean)) WHERE id=$1`, account.ID, enabled)
		require.NoError(t, err)
		snapshot, err := primary.Export(ctx, peer, nil, nil)
		require.NoError(t, err)
		found := false
		for _, row := range snapshot.Rows {
			if row.Table == "accounts" && row.Key == jsonNumber(account.ID) {
				found = true
			}
		}
		require.Equal(t, enabled, found, "only the account switch grants credentials")
		require.NoError(t, secondary.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Rows: snapshot.Rows, Version: "switch"}))
		require.Equal(t, enabled, secondary.MatchLocalModel(key.Key, "deepseek-custom-small", "responses"))
		require.False(t, secondary.MatchLocalModel(key.Key, "gpt-5", "responses"))
		var active bool
		require.NoError(t, pdb.QueryRow(`SELECT status='active' AND schedulable AND deleted_at IS NULL AND extra->'unrelated'='true'::jsonb FROM accounts WHERE id=$1`, account.ID).Scan(&active))
		require.True(t, active, "export/revocation must never disable or delete the primary account")
	}
}

func TestSiteDelayedSubscriptionBillingUsesBeijingPeriods(t *testing.T) {
	ctx := context.Background()
	db, client := newBillingAuditDatabase(t)
	u := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@example.com", PasswordHash: "hash", Balance: 100})
	group := mustCreateGroup(t, client, &service.Group{Name: "subscription-" + uuid.NewString(), Platform: service.PlatformDeepseek, SubscriptionType: "subscription"})
	var id int64
	// Monday/month boundary: previous day's event must not consume the new
	// day/week/month even if it arrives after the counters have rolled over.
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.FixedZone("Beijing", 8*3600))
	require.NoError(t, db.QueryRow(`INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at,status,daily_window_start,weekly_window_start,monthly_window_start,daily_usage_usd,weekly_usage_usd,monthly_usage_usd) VALUES($1,$2,$3,$4,'active',$5,$5,$5,3,4,5) RETURNING id`, u.ID, group.ID, now.AddDate(0, -1, 0), now.AddDate(1, 0, 0), now).Scan(&id))
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, incrementSiteSubscription(ctx, tx, id, 2, now.Add(-time.Minute)))
	require.NoError(t, tx.Commit())
	var day, week, month float64
	require.NoError(t, db.QueryRow(`SELECT daily_usage_usd,weekly_usage_usd,monthly_usage_usd FROM user_subscriptions WHERE id=$1`, id).Scan(&day, &week, &month))
	require.Equal(t, []float64{3, 4, 5}, []float64{day, week, month})
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, incrementSiteSubscription(ctx, tx, id, 2, now.Add(time.Minute)))
	require.NoError(t, tx.Commit())
	require.NoError(t, db.QueryRow(`SELECT daily_usage_usd,weekly_usage_usd,monthly_usage_usd FROM user_subscriptions WHERE id=$1`, id).Scan(&day, &week, &month))
	require.Equal(t, []float64{5, 6, 7}, []float64{day, week, month})
}
