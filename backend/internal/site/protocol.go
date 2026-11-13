package site

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Row keys and table names are validated against a fixed repository allowlist.
// RawMessage preserves PostgreSQL decimal precision across the control plane.
type Row struct {
	Table string          `json:"table"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value,omitempty"`
}

func (r Row) Identity() string { return r.Table + ":" + r.Key }

type Event struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type SyncRequest struct {
	Protocol      int     `json:"protocol"`
	NodeID        string  `json:"node_id"`
	Version       string  `json:"version"`
	Schema        string  `json:"schema"`
	Events        []Event `json:"events,omitempty"`
	PendingIDs    []int64 `json:"pending_ids,omitempty"`
	TargetVersion string  `json:"target_version,omitempty"`
	Cursor        string  `json:"cursor,omitempty"`
}

type SyncResponse struct {
	Protocol     int         `json:"protocol"`
	PrimaryID    string      `json:"primary_id"`
	NodeID       string      `json:"node_id"`
	BaseVersion  string      `json:"base_version"`
	Version      string      `json:"version"`
	Schema       string      `json:"schema"`
	Full         bool        `json:"full"`
	Rows         []Row       `json:"rows"`
	Acknowledged []int64     `json:"acknowledged"`
	Rejected     []Rejection `json:"rejected,omitempty"`
	Cursor       string      `json:"cursor,omitempty"`
}

type Snapshot struct {
	Schema       string
	Rows         []Row
	Acknowledged []int64
	Rejected     []Rejection
	Version      string
	Full         bool
	Cursor       string
}

// Store performs all monetary mutations, receipt acknowledgements and replica
// installation transactionally. Network handlers never implement billing.
type Store interface {
	Export(context.Context, Peer, []Event, []int64) (Snapshot, error)
	Pending(context.Context, int) ([]Event, error)
	PendingIDs(context.Context) ([]int64, error)
	Install(context.Context, SyncResponse) error
	Schema() string
	MatchLocalModel(key, model, endpoint string) bool
}

func snapshotVersion(s Snapshot) string {
	h := sha256.New()
	_, _ = h.Write([]byte(s.Schema))
	rows := append([]Row(nil), s.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Identity() < rows[j].Identity() })
	for _, r := range rows {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", r.Identity(), r.Value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func diffRows(previous, current []Row) []Row {
	old := make(map[string]Row, len(previous))
	for _, r := range previous {
		old[r.Identity()] = r
	}
	var diff []Row
	for _, r := range current {
		before, exists := old[r.Identity()]
		if !exists || string(before.Value) != string(r.Value) {
			diff = append(diff, r)
		}
		delete(old, r.Identity())
	}
	for _, r := range old {
		r.Value = nil
		diff = append(diff, r)
	}
	sort.Slice(diff, func(i, j int) bool { return diff[i].Identity() < diff[j].Identity() })
	return diff
}

// Receipt rejections are durable evidence, never silently accepted charges.
type Rejection struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}
type DeliveryResponse struct {
	Protocol     int         `json:"protocol"`
	NodeID       string      `json:"node_id"`
	Acknowledged []int64     `json:"acknowledged"`
	Rejected     []Rejection `json:"rejected,omitempty"`
}
type DeliveryStore interface {
	Deliver(context.Context, Peer, []Event) (DeliveryResponse, error)
	MarkDelivered(context.Context, DeliveryResponse) error
	ExportPage(context.Context, Peer, SyncRequest) (Snapshot, error)
}

const MaxPendingBills = 4096
const DeliveryBatch = 512
