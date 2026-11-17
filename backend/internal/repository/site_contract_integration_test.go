//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteContractLegacyAccountSemanticsStayOnPrimary(t *testing.T) {
	p := newSiteFollowupPair(t)
	legacy := ""
	for schema := range siteLegacySchemas {
		legacy = schema
		break
	}
	for _, tc := range []struct {
		name, platform, credentials string
		legacyEnabled               bool
	}{
		{"ordinary", service.PlatformDeepseek, `{}`, true},
		{"new_platform", service.PlatformQwen, `{}`, false},
		{"old_cooldown", service.PlatformDeepseek, `{"temp_unschedulable_enabled":true,"temp_unschedulable_rules":[{"error_code":502,"keywords":["busy"],"duration_minutes":5}]}`, true},
		{"counted_cooldown", service.PlatformDeepseek, `{"temp_unschedulable_enabled":true,"temp_unschedulable_rules":[{"error_code":502,"keywords":["busy"],"duration_minutes":5,"trigger_count":3,"trigger_window_minutes":5}]}`, false},
		{"stream_cooldown", service.PlatformDeepseek, `{"temp_unschedulable_enabled":true,"temp_unschedulable_rules":[{"error_code":502,"keywords":["busy"],"duration_minutes":5,"stream_timing":"before_stream"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.db.Exec(`UPDATE accounts SET platform=$2,credentials=$3::jsonb WHERE id=$1`, p.account, tc.platform, tc.credentials)
			require.NoError(t, err)
			for _, schema := range []string{legacy, siteConfigSchema} {
				snap, err := p.primary.ExportPage(context.Background(), p.peer, site.SyncRequest{Schema: schema})
				require.NoError(t, err)
				found := false
				for _, row := range snap.Rows {
					if row.Table != "accounts" || row.Key != strconv.FormatInt(p.account, 10) {
						continue
					}
					var account service.Account
					require.NoError(t, json.Unmarshal(row.Value, &account))
					want := schema == siteConfigSchema || tc.legacyEnabled
					require.Equal(t, want, account.SecondaryEnabled())
					require.Equal(t, want, account.Schedulable)
					found = true
				}
				require.True(t, found)
			}
			var enabled, schedulable bool
			require.NoError(t, p.db.QueryRow(`SELECT extra->'site_secondary_enabled'='true'::jsonb,schedulable FROM accounts WHERE id=$1`, p.account).Scan(&enabled, &schedulable))
			require.True(t, enabled)
			require.True(t, schedulable)
		})
	}
}

func TestSiteContractReviewedColumnsAndCachePricing(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	// CI must flag a new physical field for review without making production
	// installation depend on physical column order or unreviewed additions.
	for table, columns := range siteContractColumns {
		rows, err := p.db.Query(`SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1`, table)
		require.NoError(t, err)
		known := map[string]bool{}
		for _, field := range columns {
			known[field.Name] = true
		}
		if table == "settings" {
			// Settings are keyed by their reviewed string key on the wire;
			// local surrogate IDs and audit timestamps carry no policy.
			known["id"], known["created_at"], known["updated_at"] = true, true, true
		}
		var unclassified []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			if !known[name] {
				unclassified = append(unclassified, table+"."+name)
			}
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Empty(t, unclassified, "classify newly added site fields before rebasing")
	}
	var channelID, priceID int64
	_, err := p.db.Exec(`UPDATE accounts SET platform='openai' WHERE id=$1`, p.account)
	require.NoError(t, err)
	require.NoError(t, p.db.QueryRow(`INSERT INTO channels(name) VALUES('site-12h-contract') RETURNING id`).Scan(&channelID))
	require.NoError(t, p.db.QueryRow(`INSERT INTO channel_model_pricing(channel_id,cache_write_12h_price) VALUES($1,0.000024) RETURNING id`, channelID).Scan(&priceID))
	legacy := ""
	for schema := range siteLegacySchemas {
		legacy = schema
		break
	}
	old, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: legacy})
	require.NoError(t, err)
	for _, row := range old.Rows {
		require.NotContains(t, string(row.Value), "cache_write_12h_price")
		if row.Table == "accounts" {
			var a service.Account
			require.NoError(t, json.Unmarshal(row.Value, &a))
			require.False(t, a.SecondaryEnabled())
			require.False(t, a.Schedulable)
		}
	}
	current, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: siteConfigSchema, Version: old.Version})
	require.NoError(t, err)
	require.True(t, current.Full)
	require.NoError(t, p.local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: current.Version, Rows: current.Rows}))
	var price float64
	require.NoError(t, p.replica.QueryRow(`SELECT cache_write_12h_price FROM channel_model_pricing WHERE id=$1`, priceID).Scan(&price))
	require.Equal(t, 0.000024, price)
	var enabled bool
	require.NoError(t, p.replica.QueryRow(`SELECT schedulable AND extra->'site_secondary_enabled'='true'::jsonb FROM accounts WHERE id=$1`, p.account).Scan(&enabled))
	require.True(t, enabled)
}

func TestSiteContractLegacyProjectionUpgradeAndNewPermission(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	_, err := p.db.Exec(`ALTER TABLE users ADD COLUMN future_optional_setting text DEFAULT 'not-on-wire'`)
	require.NoError(t, err)
	_, err = p.db.Exec(`UPDATE api_keys SET context_token_limit=8192 WHERE id=$1`, p.key)
	require.NoError(t, err)
	legacy := ""
	for schema := range siteLegacySchemas {
		legacy = schema
		break
	}
	old, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: legacy})
	require.NoError(t, err)
	require.True(t, old.Full)
	require.Equal(t, legacy, old.Schema)
	for _, row := range old.Rows {
		require.NotContains(t, string(row.Value), "future_optional_setting")
		require.NotEqual(t, "account_performance_monitors", row.Table)
		if row.Table == "api_keys" && row.Key == strconv.FormatInt(p.key, 10) {
			var value map[string]any
			require.NoError(t, json.Unmarshal(row.Value, &value))
			require.NotContains(t, value, "context_token_limit")
			require.Equal(t, "active", value["status"])
			require.Equal(t, "!site-legacy-unsupported:"+strconv.FormatInt(p.key, 10), value["key"])
		}
	}
	var status string
	require.NoError(t, p.db.QueryRow(`SELECT status FROM api_keys WHERE id=$1`, p.key).Scan(&status))
	require.Equal(t, "active", status, "legacy projection must not disable the primary's key")
	upgraded, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: siteConfigSchema, Version: old.Version})
	require.NoError(t, err)
	require.True(t, upgraded.Full, "upgrading a replica must replenish newly added fields despite an advanced cursor")
	require.NotEqual(t, old.Version, upgraded.Version)
	require.NoError(t, p.local.Install(ctx, site.SyncResponse{Protocol: site.ProtocolVersion, Full: true, PrimaryID: "main", Version: upgraded.Version, Rows: upgraded.Rows}))
	var limit int
	require.NoError(t, p.replica.QueryRow(`SELECT context_token_limit,status FROM api_keys WHERE id=$1`, p.key).Scan(&limit, &status))
	require.Equal(t, 8192, limit)
	require.Equal(t, "active", status)
	heartbeat, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: siteConfigSchema, Version: upgraded.Version})
	require.NoError(t, err)
	require.False(t, heartbeat.Full)
	require.Empty(t, heartbeat.Rows)
}

func TestSiteContractUnknownOptionalAndMissingRequiredFields(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	snapshot, err := p.primary.Export(ctx, p.peer, nil, nil)
	require.NoError(t, err)
	var userIndex int
	for i := range snapshot.Rows {
		r := &snapshot.Rows[i]
		if r.Table == "users" {
			userIndex = i
		}
		if r.Table == "api_keys" {
			var obj map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(r.Value, &obj))
			delete(obj, "context_token_limit")
			obj["future_optional"] = json.RawMessage(`{"enabled":true}`)
			r.Value, err = json.Marshal(obj)
			require.NoError(t, err)
		}
	}
	install := func(rows []site.Row) error {
		return p.local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: "contract-test", Rows: rows})
	}
	require.NoError(t, install(snapshot.Rows))
	var limit int
	require.NoError(t, p.replica.QueryRow(`SELECT context_token_limit FROM api_keys WHERE id=$1`, p.key).Scan(&limit))
	require.Zero(t, limit)
	for _, missing := range []bool{true, false} {
		rows := append([]site.Row(nil), snapshot.Rows...)
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rows[userIndex].Value, &obj))
		if missing {
			delete(obj, "balance")
		} else {
			obj["balance"] = json.RawMessage(`null`)
		}
		rows[userIndex].Value, err = json.Marshal(obj)
		require.NoError(t, err)
		require.ErrorContains(t, install(rows), "users.balance")
		p.balance(t, "100.00000000", "100.00000000")
	}
}

func TestSiteContractUltrafastBillingAndProxyIsolation(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	_, err := p.db.Exec(`UPDATE groups SET free_openai_ultrafast=true WHERE id=(SELECT group_id FROM api_keys WHERE id=$1)`, p.key)
	require.NoError(t, err)
	_, err = p.db.Exec(`UPDATE accounts SET extra=extra||'{"openai_proxy_mode":"request","openai_proxy_ids":[41],"openai_excel_bps_proxy_mode":"request","openai_excel_bps_proxy_ids":[42],"request_gzip_enabled":true}'::jsonb WHERE id=$1`, p.account)
	require.NoError(t, err)
	p.sync(t)
	var enabled bool
	require.NoError(t, p.replica.QueryRow(`SELECT free_openai_ultrafast FROM groups WHERE id=(SELECT group_id FROM api_keys WHERE id=$1)`, p.key).Scan(&enabled))
	require.True(t, enabled)
	var extra string
	require.NoError(t, p.replica.QueryRow(`SELECT extra::text FROM accounts WHERE id=$1`, p.account).Scan(&extra))
	require.NotContains(t, extra, "proxy")
	require.Contains(t, extra, "request_gzip_enabled")
	require.Contains(t, extra, "site_secondary_enabled")
	require.NoError(t, p.db.QueryRow(`SELECT extra ? 'openai_proxy_ids' FROM accounts WHERE id=$1`, p.account).Scan(&enabled))
	require.True(t, enabled, "export must not change primary network configuration")

	snapshot, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{Schema: siteConfigSchema})
	require.NoError(t, err)
	for i := range snapshot.Rows {
		row := &snapshot.Rows[i]
		if row.Table != "groups" {
			continue
		}
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(row.Value, &obj))
		delete(obj, "free_openai_ultrafast")
		obj["future_optional_billing_ui"] = json.RawMessage(`true`)
		row.Value, err = json.Marshal(obj)
		require.NoError(t, err)
	}
	require.NoError(t, p.local.Install(ctx, site.SyncResponse{Full: true, PrimaryID: "main", Version: "old-policy", Rows: snapshot.Rows}))
	require.NoError(t, p.replica.QueryRow(`SELECT free_openai_ultrafast FROM groups WHERE id=(SELECT group_id FROM api_keys WHERE id=$1)`, p.key).Scan(&enabled))
	require.False(t, enabled, "an absent optional field explicitly restores the pre-upgrade billing policy")
}

func TestSiteContractOldBillsOptionalDetailsAndRequiredDebit(t *testing.T) {
	p := newSiteFollowupPair(t)
	event := p.bill(t)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(event.Payload, &envelope))
	var usage map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["usage"], &usage))
	for _, key := range []string{"Platform", "CustomerCostDetails", "CacheCreation30mTokens", "CacheCreation12hTokens"} {
		delete(usage, key)
	}
	var err error
	envelope["usage"], err = json.Marshal(usage)
	require.NoError(t, err)
	event.Payload, err = json.Marshal(envelope)
	require.NoError(t, err)
	digest := site.EventDigest(event)
	out, err := p.primary.Deliver(context.Background(), p.peer, []site.Event{event})
	require.NoError(t, err)
	require.Equal(t, []int64{event.ID}, out.Acknowledged)
	require.Equal(t, digest, out.ReceiptDigests[event.ID])
	out, err = p.primary.Deliver(context.Background(), p.peer, []site.Event{event})
	require.NoError(t, err)
	require.Equal(t, []int64{event.ID}, out.Acknowledged)
	p.balance(t, "99.00000000", "99.00000000")
	var command map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["command"], &command))
	delete(command, "BalanceCost")
	envelope["command"], err = json.Marshal(command)
	require.NoError(t, err)
	bad, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.ErrorContains(t, validateSiteBillingFields(bad), "BalanceCost")
}
