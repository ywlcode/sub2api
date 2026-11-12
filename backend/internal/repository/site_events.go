package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

// Request diagnostics on a secondary cross the local durable boundary before
// the handler returns. Network upload remains asynchronous via the DB trigger.
func (s *SiteStore) RecordConnectionTrace(record service.ConnectionTrace) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := &usageLogRepository{db: s.db}
	if err := r.writeConnectionTrace(ctx, record); err != nil {
		slog.Error("site connection trace persistence failed", "request_id", record.RequestID, "error", err)
	}
}

func (r *usageBillingRepository) RecordSiteUsageIssue(ctx context.Context, issue service.SiteUsageIssue) error {
	if !r.secondary {
		return errors.New("site reconciliation is secondary-only")
	}
	// This is an unsettled bill, not evidence that the client received an HTTP
	// error. Leave the unknown HTTP status NULL instead of inventing a 502.
	_, err := r.db.ExecContext(ctx, `INSERT INTO ops_error_logs(request_id,user_id,api_key_id,account_id,model,error_phase,error_type,error_message) VALUES($1,$2,$3,$4,$5,'billing','site_usage_unsettled',$6)`, issue.RequestID, issue.UserID, issue.APIKeyID, issue.AccountID, issue.Model, issue.Reason)
	return err
}

func (s *SiteStore) importEvent(ctx context.Context, tx *sql.Tx, p site.Peer, event site.Event) (*service.UsageBillingEvent, error) {
	if event.ID <= 0 || len(event.Payload) == 0 {
		return nil, errors.New("invalid site event")
	}
	digest := sha256.Sum256(append([]byte(event.Kind+"\x00"), event.Payload...))
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT digest FROM site_receipts WHERE node_id=$1 AND event_id=$2`, p.ID, event.ID).Scan(&existing)
	duplicate := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if duplicate && existing != hex.EncodeToString(digest[:]) {
		return nil, errors.New("site event identity conflict")
	}
	var billing *service.UsageBillingEvent
	switch event.Kind {
	case "billing":
		var e service.UsageBillingEvent
		if err = json.Unmarshal(event.Payload, &e); err != nil {
			return nil, err
		}
		c, u := &e.Command, &e.Usage
		sourceRequestID := c.RequestID
		if err = validateUsageBillingEvent(&e, service.UsageBillingEventKey{RequestID: c.RequestID, APIKeyID: c.APIKeyID}, c.RequestFingerprint); err != nil {
			return nil, err
		}
		var allowed bool
		// Retain past grants so a response already in flight when the switch
		// was turned off can still settle exactly once.
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM site_account_grants WHERE node_id=$1 AND account_id=$2)`, p.ID, c.AccountID).Scan(&allowed); err != nil {
			return nil, err
		}

		if !allowed || c.UserID <= 0 || c.APIKeyID <= 0 || c.RequestID == "" || len(c.RequestID) > 128 || u.CreatedAt.After(time.Now().Add(time.Minute)) {
			return nil, errors.New("site billing ownership or time invalid")
		}
		for _, v := range []float64{c.BalanceCost, c.SubscriptionCost, c.APIKeyQuotaCost, c.APIKeyRateLimitCost, c.AccountQuotaCost, c.PlatformQuotaCost, u.ActualCost, u.TotalCost} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return nil, errors.New("invalid site billing amount")
			}
		}
		// The peer is trusted to quote using its synchronized pricing. Check
		// owner relationships, but do not reprice an old request with new rules.
		var owner bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys WHERE id=$1 AND user_id=$2)`, c.APIKeyID, c.UserID).Scan(&owner); err != nil {
			return nil, err
		}
		if !owner {
			return nil, errors.New("site billing key owner mismatch")
		}
		if c.SubscriptionID != nil {
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_subscriptions WHERE id=$1 AND user_id=$2 AND group_id=$3)`, *c.SubscriptionID, c.UserID, u.GroupID).Scan(&owner); err != nil {
				return nil, err
			}
			if !owner {
				return nil, errors.New("site subscription owner mismatch")
			}
		}
		billing = &e
		if !duplicate {
			// The peer namespace protects primary requests and other replicas from
			// client-supplied request ID collisions. Original trace IDs remain in
			// the diagnostic records; upstream IDs remain on the usage row.
			c.RequestID = siteRequestID(p.ID, c.RequestID)
			u.RequestID = c.RequestID
			repo := &usageBillingRepository{db: s.db}
			applied, err := repo.claimUsageBillingKey(ctx, tx, c)
			if err != nil {
				return nil, err
			}
			if applied {
				if err = replaySiteDebit(ctx, tx, &e); err != nil {
					return nil, err
				}
				appliedResult := service.UsageBillingApplyResult{Applied: true, APIKeyQuotaExhausted: c.APIKeyQuotaCost > 0}
				if c.BalanceCost > 0 {
					if err = advanceAutoCreditAfterDebit(ctx, tx, c, &appliedResult); err != nil {
						return nil, err
					}
				}
				if err = insertUsageBillingQuote(ctx, tx, c); err != nil {
					return nil, err
				}
				if err = execUsageLogInsertNoResult(ctx, tx, prepareUsageLogInsert(u)); err != nil {
					return nil, err
				}
				// Keep the existing durable cache recovery worker responsible after
				// a primary crash between SQL commit and cache invalidation.
				payload, marshalErr := json.Marshal(e)
				if marshalErr != nil {
					return nil, marshalErr
				}
				result, marshalErr := json.Marshal(appliedResult)
				if marshalErr != nil {
					return nil, marshalErr
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO usage_billing_events(request_id,api_key_id,request_fingerprint,schema_version,payload,observed_at,state,result,applied_at) VALUES($1,$2,$3,1,$4::jsonb,$5,'applied',$6::jsonb,NOW())`, c.RequestID, c.APIKeyID, c.RequestFingerprint, string(payload), u.CreatedAt, string(result))
				if err != nil {
					return nil, err
				}
			}
		}
		if err = linkSiteRequestTimings(ctx, tx, p.ID, sourceRequestID, c.APIKeyID); err != nil {
			return nil, err
		}
	case "ops_error_logs", "request_connection_traces":
		if !duplicate {
			if err = s.importLog(ctx, tx, p.ID, event.Kind, event.Payload); err != nil {
				return nil, err
			}
		}
	default:
		return nil, errors.New("unknown site event kind")
	}
	if !duplicate {
		_, err = tx.ExecContext(ctx, `INSERT INTO site_receipts(node_id,event_id,digest) VALUES($1,$2,$3)`, p.ID, event.ID, hex.EncodeToString(digest[:]))
		if err != nil {
			return nil, err
		}
	}
	return billing, nil
}

// Replay both installs the local pending overlay and settles the same immutable
// bill on the primary. It never creates auto-credit on a replica and does not
// reset historical usage to the period in which delivery happened.
func replaySiteDebit(ctx context.Context, tx *sql.Tx, e *service.UsageBillingEvent) error {
	c := &e.Command
	if c.BalanceCost > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=balance-$1 WHERE id=$2`, c.BalanceCost, c.UserID); err != nil {
			return err
		}
	}
	if c.APIKeyQuotaCost > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET quota_used=quota_used+$1,status=CASE WHEN quota>0 AND quota_used+$1>=quota AND status='active' THEN 'quota_exhausted' ELSE status END WHERE id=$2`, c.APIKeyQuotaCost, c.APIKeyID); err != nil {
			return err
		}
	}
	if c.APIKeyRateLimitCost > 0 {
		if err := incrementSiteKeyWindows(ctx, tx, c.APIKeyID, c.APIKeyRateLimitCost, e.Usage.CreatedAt); err != nil {
			return err
		}
	}
	if c.SubscriptionCost > 0 && c.SubscriptionID != nil {
		if err := incrementSiteSubscription(ctx, tx, *c.SubscriptionID, c.SubscriptionCost, e.Usage.CreatedAt); err != nil {
			return err
		}
	}
	if c.PlatformQuotaCost > 0 && c.PlatformQuotaPlatform != "" {
		if err := incrementUsageBillingPlatformQuota(ctx, tx, c); err != nil {
			return err
		}
	}
	if c.AccountQuotaCost > 0 {
		if _, err := incrementUsageBillingAccountQuota(ctx, tx, c.AccountID, c.AccountQuotaCost, c.AccountQuotaHasDailyLimit, c.AccountQuotaHasWeeklyLimit); err != nil {
			return err
		}
	}
	return nil
}

func incrementSiteSubscription(ctx context.Context, tx *sql.Tx, id int64, cost float64, at time.Time) error {
	starts := service.SubscriptionWindowStarts(at)
	var clauses []string
	args := []any{id, cost, at, starts[0], starts[1], starts[2]}
	for i, window := range []string{"daily", "weekly", "monthly"} {
		start := window + "_window_start"
		usage := window + "_usage_usd"
		boundary := fmt.Sprintf("$%d::timestamptz", i+4)
		clauses = append(clauses, usage+`=CASE WHEN `+start+`>$3::timestamptz THEN `+usage+` WHEN `+start+` IS NULL OR `+start+`<`+boundary+` THEN $2::numeric ELSE `+usage+`+$2::numeric END`, start+`=GREATEST(`+start+`,`+boundary+`)`)
	}
	_, err := tx.ExecContext(ctx, `UPDATE user_subscriptions SET `+strings.Join(clauses, ",")+` WHERE id=$1`, args...)
	return err
}

func incrementSiteKeyWindows(ctx context.Context, tx *sql.Tx, id int64, cost float64, at time.Time) error {
	var clauses []string
	for _, w := range []struct{ name, interval string }{{"5h", "5 hours"}, {"1d", "1 day"}, {"7d", "7 days"}} {
		start := "window_" + w.name + "_start"
		usage := "usage_" + w.name
		clauses = append(clauses, usage+`=CASE WHEN `+start+`>$3::timestamptz THEN `+usage+` WHEN `+start+` IS NULL OR `+start+`+INTERVAL '`+w.interval+`'<=$3::timestamptz THEN $2::numeric ELSE `+usage+`+$2::numeric END`, start+`=CASE WHEN `+start+` IS NULL OR `+start+`+INTERVAL '`+w.interval+`'<=$3::timestamptz THEN $3::timestamptz ELSE `+start+` END`)
	}
	_, err := tx.ExecContext(ctx, `UPDATE api_keys SET `+strings.Join(clauses, ",")+` WHERE id=$1`, id, cost, at)
	return err
}

func (s *SiteStore) importLog(ctx context.Context, tx *sql.Tx, node, table string, raw []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	var requestID string
	_ = json.Unmarshal(obj["request_id"], &requestID)
	if requestID != "" {
		obj["request_id"], _ = json.Marshal(siteRequestID(node, requestID))
	}
	pk := "id"
	if table == "request_connection_traces" {
		pk = "trace_id"
	}
	source := strings.Trim(string(obj[pk]), `"`)
	if source == "" {
		return errors.New("missing site log ID")
	}
	var local string
	err := tx.QueryRowContext(ctx, `SELECT local_id FROM site_imported_logs WHERE node_id=$1 AND table_name=$2 AND source_id=$3`, node, table, source).Scan(&local)
	if errors.Is(err, sql.ErrNoRows) {
		if pk == "id" {
			var id int64
			if err = tx.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('ops_error_logs','id'))`).Scan(&id); err != nil {
				return err
			}
			local = strconv.FormatInt(id, 10)
		} else {
			local = source
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO site_imported_logs(node_id,table_name,source_id,local_id) VALUES($1,$2,$3,$4)`, node, table, source, local); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if pk == "id" {
		obj[pk] = json.RawMessage(local)
	} else {
		obj[pk], _ = json.Marshal(local)
		// Reuse the application trace format/UI; site identity is additional
		// metadata and must not hide timing detail behind an unknown source.
		obj["source"], _ = json.Marshal("application")
		var detail map[string]json.RawMessage
		_ = json.Unmarshal(obj["detail"], &detail)
		if detail == nil {
			detail = map[string]json.RawMessage{}
		}
		detail["site_node_id"], _ = json.Marshal(node)
		var timing requesttiming.Snapshot
		if err := json.Unmarshal(obj["detail"], &timing); err != nil {
			return err
		}
		detail["latency"], _ = json.Marshal(requesttiming.DeriveLatency(timing))
		obj["detail"], _ = json.Marshal(detail)
	}
	// SQL column names are obtained from the local catalog, never the payload.
	rows, err := tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`, table)
	if err != nil {
		return err
	}
	var columns, updates []string
	for rows.Next() {
		var col string
		if err = rows.Scan(&col); err != nil {
			_ = rows.Close()
			return err
		}
		columns = append(columns, col)
		if col != pk {
			updates = append(updates, `"`+col+`"=EXCLUDED."`+col+`"`)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO "`+table+`" (`+quotedColumns(columns)+`) SELECT `+quotedColumns(columns)+` FROM jsonb_populate_record(NULL::"`+table+`",$1::jsonb) ON CONFLICT("`+pk+`") DO UPDATE SET `+strings.Join(updates, ","), string(encoded))
	if err == nil && table == "request_connection_traces" {
		var clientID string
		var apiKeyID int64
		_ = json.Unmarshal(obj["client_request_id"], &clientID)
		_ = json.Unmarshal(obj["api_key_id"], &apiKeyID)
		billingID := "local:" + requestID
		if clientID != "" {
			billingID = "client:" + clientID
		}
		err = linkSiteRequestTimings(ctx, tx, node, billingID, apiKeyID)
	}
	return err
}

func siteRequestID(node, requestID string) string {
	requestID = strings.TrimPrefix(requestID, "local:")
	sum := sha256.Sum256([]byte(node + "\x00" + requestID))
	return "site:" + hex.EncodeToString(sum[:28])
}
