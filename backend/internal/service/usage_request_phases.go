package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

func (s *UsageService) requestPhaseAverages(ctx context.Context, filters usagestats.UsageLogFilters) usagestats.RequestPhaseAverages {
	// Admin overview, user pages, and key-only dashboards share the same rolling
	// window. Key-only dashboards never broaden their scope to the owner.
	filters = requestPhaseScope(filters)
	if s.phaseCache != nil {
		return s.phaseCache.Get(ctx, filters)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	window := requestPhaseWindow(filters, time.Now())
	var stats usagestats.RequestPhaseAverages
	var err error
	if filters.UserID == 0 && filters.APIKeyID == 0 {
		global, ok := s.usageRepo.(globalRequestPhaseHourlyReader)
		if !ok {
			return usagestats.RequestPhaseAverages{}
		}
		stats, err = global.ReadGlobalRequestPhaseHourlyAverages(ctx, *window.StartTime, *window.EndTime)
	} else {
		reader, ok := s.usageRepo.(requestPhaseReader)
		if !ok {
			return usagestats.RequestPhaseAverages{}
		}
		stats, err = reader.ReadUsageRequestPhaseAverages(ctx, window)
	}
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
