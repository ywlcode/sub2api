package service

import (
	"context"
	"log/slog"
	"time"
)

type moderationBanExpiryRepository interface {
	ExpireModerationBans(context.Context, int) ([]int64, error)
}

// The provider starts this once, only on nodes that run global jobs. Deadlines
// must be processed even when content auditing has since been switched off.
func (s *ContentModerationService) startBanExpiry() {
	repo, ok := s.repo.(moderationBanExpiryRepository)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.banExpiryCancel = cancel
	s.banExpiryDone = make(chan struct{})
	go func() {
		defer close(s.banExpiryDone)
		const batch = 64
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			run, end := context.WithTimeout(ctx, 15*time.Second)
			ids, err := repo.ExpireModerationBans(run, batch)
			end()
			if err != nil && ctx.Err() == nil {
				slog.Warn("content_moderation.expire_bans_failed", "error", err)
			}
			// users.status updates also enqueue durable auth-cache invalidations.
			for _, id := range ids {
				if s.authCacheInvalidator != nil {
					s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, id)
				}
			}
			if len(ids) == batch && err == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *ContentModerationService) StopBanExpiry() {
	if s.banExpiryCancel != nil {
		s.banExpiryCancel()
		<-s.banExpiryDone
	}
}
