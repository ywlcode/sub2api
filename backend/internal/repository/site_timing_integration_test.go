//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSiteTimingReconcilesArrivalOrderAndClientRetries(t *testing.T) {
	ctx := context.Background()
	db, client := newBillingAuditDatabase(t)
	localDB, _ := newBillingAuditDatabase(t)
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@example.com", PasswordHash: "disabled", Balance: 10})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: uuid.NewString(), Name: "timing"})
	account := mustCreateAccount(t, client, &service.Account{Name: "domestic", Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true}})
	peer := site.Peer{ID: "office", Token: strings.Repeat("s", 32)}
	primary, err := NewSiteStore(db, &config.Config{Site: site.Config{Mode: site.Primary, ID: "main"}})
	require.NoError(t, err)
	local, err := NewSiteStore(localDB, &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}})
	require.NoError(t, err)
	snapshot, err := primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: true, Version: "1", Rows: snapshot.Rows}))
	billing := &usageBillingRepository{db: localDB, secondary: true}
	for _, clientIDUsed := range []bool{false, true} {
		for _, traceFirst := range []bool{false, true} {
			requestID, clientID := uuid.NewString(), ""
			billingID := "local:" + requestID
			if clientIDUsed {
				clientID = uuid.NewString()
				billingID = "client:" + clientID
			}
			cmd := service.UsageBillingCommand{RequestID: billingID, APIKeyID: key.ID, UserID: user.ID, AccountID: account.ID, Model: "deepseek-chat", BalanceCost: 0.01}
			usage := service.UsageLog{RequestID: billingID, APIKeyID: key.ID, UserID: user.ID, AccountID: account.ID, Model: cmd.Model, CreatedAt: time.Now().UTC(), ActualCost: 0.01, TotalCost: 0.01}
			require.NoError(t, billing.StageUsageBilling(ctx, &cmd, &usage))
			_, err = billing.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: billingID, APIKeyID: key.ID})
			require.NoError(t, err)
			traceID := uuid.NewString()
			local.RecordConnectionTrace(service.ConnectionTrace{TraceID: traceID, StartedAt: time.Now(), RequestID: requestID, ClientRequestID: clientID, APIKeyID: key.ID, UserID: user.ID, AccountID: account.ID, Source: "application", Detail: requesttiming.Snapshot{TraceID: traceID}})
			events, err := local.Pending(ctx, 128)
			require.NoError(t, err)
			require.Len(t, events, 2)
			if traceFirst {
				events[0], events[1] = events[1], events[0]
			}
			// A different site can have the same client retry ID, but not the
			// same generated trace UUID. Its timing must not attach to our bill.
			var otherTrace site.Event
			for _, event := range events {
				if event.Kind == "request_connection_traces" {
					otherTrace = event
				}
			}
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(otherTrace.Payload, &fields))
			fields["trace_id"], _ = json.Marshal(uuid.NewString())
			otherTrace.Payload, err = json.Marshal(fields)
			require.NoError(t, err)
			_, err = primary.Export(ctx, site.Peer{ID: "other", Token: strings.Repeat("t", 32)}, []site.Event{otherTrace}, nil)
			require.NoError(t, err)
			ids := []int64{events[0].ID, events[1].ID}
			for _, event := range events {
				_, err = primary.Export(ctx, peer, []site.Event{event}, ids)
				require.NoError(t, err)
			}
			// Redelivery cannot duplicate timing samples or billing.
			snapshot, err = primary.Export(ctx, peer, events, ids)
			require.NoError(t, err)
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM request_timing_details d JOIN usage_logs u ON u.id=d.usage_log_id WHERE u.request_id=$1 AND d.detail ? 'latency'`, siteRequestID(peer.ID, billingID)).Scan(&count))
			require.Equal(t, 1, count, "timing must join for either arrival order, without cross-site matches or duplicate samples")
			require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: true, Version: uuid.NewString(), Rows: snapshot.Rows, Acknowledged: snapshot.Acknowledged}))
		}
	}
}
