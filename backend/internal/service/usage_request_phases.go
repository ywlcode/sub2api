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
	reader, ok := s.usageRepo.(interface {
		ReadUsageRequestPhaseAverages(context.Context, usagestats.UsageLogFilters) (usagestats.RequestPhaseAverages, error)
	})
	if !ok {
		return usagestats.RequestPhaseAverages{}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stats, err := reader.ReadUsageRequestPhaseAverages(ctx, filters)
	if err != nil {
		// Diagnostics must not make the billing dashboard unavailable.
		return usagestats.RequestPhaseAverages{}
	}
	return stats
}
