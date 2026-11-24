package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// User-visible records contain only the retained excerpt. Admin notes, provider
// responses, evidence attachments and credentials are outside this projection.
type MyModerationRecord struct {
	ID             int64      `json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	Model          string     `json:"model"`
	Category       string     `json:"category"`
	Action         string     `json:"action"`
	InputExcerpt   string     `json:"input_excerpt"`
	ViolationCount int        `json:"violation_count"`
	BanApplied     *bool      `json:"ban_applied"`
	BanScope       string     `json:"ban_scope"`
	BanExpiresAt   *time.Time `json:"ban_expires_at,omitempty"`
}

type MyModerationRecordsPage struct {
	CurrentCount     int                  `json:"current_count"`
	Threshold        int                  `json:"threshold"`
	AutoBanEnabled   bool                 `json:"auto_ban_enabled"`
	AccountBanned    bool                 `json:"account_banned"`
	OpenAIBanned     bool                 `json:"openai_banned"`
	RecordedBanCount int64                `json:"recorded_ban_count"`
	Items            []MyModerationRecord `json:"items"`
	Total            int64                `json:"total"`
	Page             int                  `json:"page"`
	PageSize         int                  `json:"page_size"`
}

func (s *ContentModerationService) MyModerationRecords(ctx context.Context, userID int64, page, size int) (*MyModerationRecordsPage, error) {
	if userID <= 0 || page < 1 || page > 100000 || size < 1 || size > 100 {
		return nil, infraerrors.BadRequest("INVALID_MODERATION_FILTER", "Invalid user or pagination")
	}
	repo, ok := s.repo.(interface {
		MyModerationRecords(context.Context, int64, int, int, time.Time, bool) (*MyModerationRecordsPage, error)
	})
	if !ok {
		return nil, infraerrors.ServiceUnavailable("MODERATION_USER_STATE_UNAVAILABLE", "Content moderation user state unavailable")
	}
	snapshot, err := s.loadRuntimeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	cfg := snapshot.config
	result, err := repo.MyModerationRecords(ctx, userID, page, size, time.Now().Add(-time.Duration(cfg.ViolationWindowHours)*time.Hour), cfg.CyberPolicyExcludeFromBanCount)
	if err != nil {
		return nil, err
	}
	result.Threshold = cfg.BanThreshold
	_, allowlisted := snapshot.allowlistedUsers[userID]
	result.AutoBanEnabled = result.AutoBanEnabled && !allowlisted && cfg.AutoBanEnabled && snapshot.riskControlEnabled
	for i := range result.Items {
		// Old records may predate current redaction rules.
		excerpt := result.Items[i].InputExcerpt
		if excerpt == "[Recovered context withheld]" {
			excerpt = ""
		}
		result.Items[i].InputExcerpt = trimRunes(redactContentModerationSecrets(excerpt), maxModerationExcerptRunes)
	}
	return result, nil
}
