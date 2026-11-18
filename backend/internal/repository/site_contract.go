package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/lib/pq"
)

// Schema identifies required semantics, not a physical table layout. Optional
// additions keep this value; the projection revision forces existing cursors
// to receive newly reviewed fields after an upgrade.
const siteConfigSchema = "sub2api.site.config.v1"
const siteProjectionRevision = "2026-11-18.1"
const siteEventSchema = "sub2api.site.events.v1"

type siteColumn struct {
	Name, Type string
	Nullable   bool
	Optional   bool
	Default    string // JSON wire default, only used for an absent optional field
}

func (s *SiteStore) AcceptConfigSchema(schema string) bool {
	return schema == siteConfigSchema || siteLegacySchemas[schema]
}

func (s *SiteStore) AcceptEventSchema(schema string) bool {
	// Old peers used their database fingerprint for both endpoints. New event
	// delivery has its own contract and remains independent of config upgrades.
	return schema == siteEventSchema || s.AcceptConfigSchema(schema)
}

func (s *SiteStore) EventSchema() string { return siteEventSchema }

// Legacy v6 replicas predate Qwen and counted/stream-scoped cooldown rules.
// They must not silently run those accounts with different routing semantics.
func siteAccountNeedsCurrentReplica(obj map[string]json.RawMessage) bool {
	var account service.Account
	_ = json.Unmarshal(obj["platform"], &account.Platform)
	if account.Platform == service.PlatformQwen {
		return true
	}
	_ = json.Unmarshal(obj["credentials"], &account.Credentials)
	if account.IsTempUnschedulableEnabled() {
		for _, rule := range account.GetTempUnschedulableRules() {
			if rule.StreamTiming != "" || rule.NormalizedTriggerCount() > 1 {
				return true
			}
		}
	}
	return false
}

func (s *SiteStore) projectionSchema(p site.Peer) string {
	if p.ConfigSchema != "" {
		return p.ConfigSchema
	}
	return s.schema
}

func (s *SiteStore) initializeContract(ctx context.Context) error {
	s.schema = siteConfigSchema
	s.logColumns = map[string][]string{}
	// Validate the reviewed columns once. New/unrelated database columns and
	// ordinal positions never enter the wire format or its compatibility ID.
	for table, fields := range siteContractColumns {
		rows, err := s.db.QueryContext(ctx, `SELECT column_name,udt_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1`, table)
		if err != nil {
			return err
		}
		actual := map[string]string{}
		for rows.Next() {
			var name, kind string
			if err = rows.Scan(&name, &kind); err != nil {
				_ = rows.Close()
				return err
			}
			actual[name] = kind
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, field := range fields {
			if actual[field.Name] != field.Type {
				return fmt.Errorf("site contract column %s.%s requires %s (found %q)", table, field.Name, field.Type, actual[field.Name])
			}
		}
		if table == "ops_error_logs" || table == "request_connection_traces" {
			for _, f := range fields {
				s.logColumns[table] = append(s.logColumns[table], f.Name)
			}
		}
	}
	for i := range s.tables {
		t := &s.tables[i]
		t.requiredJSON = map[string]string{}
		for _, f := range siteContractColumns[t.name] {
			t.columns = append(t.columns, f.Name)
			if !f.Nullable && (f.Type == "json" || f.Type == "jsonb") {
				t.requiredJSON[f.Name] = f.Type
			}
		}
	}
	return nil
}

func siteProjectionSQL(table string, legacy bool) string {
	var pairs []string
	for _, f := range siteContractColumns[table] {
		if legacy && f.Optional {
			continue
		}
		pairs = append(pairs, pq.QuoteLiteral(f.Name)+`,t.`+pq.QuoteIdentifier(f.Name))
	}
	// jsonb_build_object accepts at most 100 arguments on supported PostgreSQL.
	var chunks []string
	for start := 0; start < len(pairs); start += 40 {
		chunks = append(chunks, "jsonb_build_object("+strings.Join(pairs[start:min(start+40, len(pairs))], ",")+")")
	}
	return "(" + strings.Join(chunks, " || ") + ")"
}

// Validate required fields before installation; ignore only unknown additive
// fields. SQL NULL, JSON null and an absent permission/amount are not equivalent.
func normalizeSiteContractRow(table string, raw json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	known := make(map[string]json.RawMessage, len(siteContractColumns[table]))
	for _, f := range siteContractColumns[table] {
		value, ok := obj[f.Name]
		if !ok {
			if !f.Optional {
				return nil, fmt.Errorf("missing site field %s.%s", table, f.Name)
			}
			value = json.RawMessage(f.Default)
		}
		if string(value) == "null" && !f.Nullable && f.Type != "json" && f.Type != "jsonb" {
			return nil, fmt.Errorf("null site field %s.%s", table, f.Name)
		}
		known[f.Name] = value
	}
	return json.Marshal(known)
}
