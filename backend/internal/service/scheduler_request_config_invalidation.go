package service

import (
	"context"
	"fmt"
	"log/slog"
)

// Durable scheduler events cover background account metadata changes and group
// binding edits as well as administrative writes. Last-used traffic is excluded.
func (s *SchedulerSnapshotService) notifyRequestConfigChange(ctx context.Context, event SchedulerOutboxEvent) {
	notifier, ok := s.cache.(interface {
		NotifyRequestConfigInvalidation(context.Context, string) error
	})
	if !ok {
		return
	}
	message := RequestConfigInvalidationPrefix + "catalog:0"
	switch event.EventType {
	case SchedulerOutboxEventGroupChanged:
		if event.GroupID == nil {
			return
		}
		message = fmt.Sprintf("%sgroup:%d", RequestConfigInvalidationPrefix, *event.GroupID)
	case SchedulerOutboxEventAccountChanged, SchedulerOutboxEventAccountGroupsChanged, SchedulerOutboxEventAccountBulkChanged:
	case SchedulerOutboxEventFullRebuild:
		message = RequestConfigInvalidationPrefix + "all:0"
	default:
		return
	}
	if err := notifier.NotifyRequestConfigInvalidation(ctx, message); err != nil {
		slog.Warn("scheduler request configuration notification failed", "error", err)
	}
}
