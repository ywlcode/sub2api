package service

import (
	"context"
	"encoding/json"
	"time"

	apperrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrSiteRejectionNotFound      = apperrors.NotFound("SITE_REJECTION_NOT_FOUND", "Site event not found")
	ErrSiteReconciliationConflict = apperrors.Conflict("SITE_RECONCILIATION_CONFLICT", "Site event already resolved differently")
	ErrSiteReconciliationBlocked  = apperrors.Conflict("SITE_RECONCILIATION_BLOCKED", "Site event still cannot be imported; fix its dependency before retrying")
)

type SiteRejectedEvent struct {
	NodeID         string          `json:"node_id"`
	EventID        int64           `json:"event_id"`
	Kind           string          `json:"kind"`
	Reason         string          `json:"reason"`
	State          string          `json:"state"`
	ResolutionNote string          `json:"resolution_note"`
	ResolvedAt     *time.Time      `json:"resolved_at"`
	ResolvedBy     *int64          `json:"resolved_by"`
	Attempts       int             `json:"attempts"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

type SiteReconciliationStore interface {
	ListSiteRejections(context.Context, string, string, int64, int) ([]SiteRejectedEvent, error)
	GetSiteRejection(context.Context, string, int64) (*SiteRejectedEvent, error)
	ResolveSiteRejection(context.Context, string, int64, string, string, int64) (*SiteRejectedEvent, error)
}
