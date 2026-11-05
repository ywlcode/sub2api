package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

func (s *UsageService) requestPhaseAverages(ctx context.Context, filters usagestats.UsageLogFilters) usagestats.RequestPhaseAverages {
	// Only user/key pages display these averages. Avoid an extra global trace
	// scan when the shared statistics service serves the admin overview.
	if filters.UserID <= 0 && filters.APIKeyID <= 0 {
		return usagestats.RequestPhaseAverages{}
	}
	// A signed-in user's diagnostic averages use their last 24 hours.
	// Key-only dashboards never broaden their authorization scope to the owner.
	filters = requestPhaseScope(filters)
	if s.phaseCache != nil {
		return s.phaseCache.Get(ctx, filters)
	}
	reader, ok := s.usageRepo.(interface {
		ReadUsageRequestPhaseAverages(context.Context, usagestats.UsageLogFilters) (usagestats.RequestPhaseAverages, error)
	})
	if !ok {
		return usagestats.RequestPhaseAverages{}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stats, err := reader.ReadUsageRequestPhaseAverages(ctx, requestPhaseWindow(filters, time.Now()))
	if err != nil {
		// Diagnostics must not make the billing dashboard unavailable.
		return usagestats.RequestPhaseAverages{}
	}
	return stats
}

func requestPhaseScope(filters usagestats.UsageLogFilters) usagestats.UsageLogFilters {
	if filters.UserID > 0 {
		return usagestats.UsageLogFilters{UserID: filters.UserID}
	}
	return usagestats.UsageLogFilters{APIKeyID: filters.APIKeyID}
}

// Diagnostics have a fixed rolling window independent of chart/list filters.
func requestPhaseWindow(scope usagestats.UsageLogFilters, now time.Time) usagestats.UsageLogFilters {
	end := now.UTC().Truncate(30 * time.Second)
	start := end.Add(-24 * time.Hour)
	scope.StartTime, scope.EndTime = &start, &end
	return scope
}
