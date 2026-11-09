package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type requestPhaseStatsRepo struct {
	UsageLogRepository
	filters usagestats.UsageLogFilters
	reads   int
	err     error
}

func (r *requestPhaseStatsRepo) GetStatsWithFilters(context.Context, usagestats.UsageLogFilters) (*usagestats.UsageStats, error) {
	return &usagestats.UsageStats{TotalRequests: 10, TotalActualCost: 7}, nil
}

func (r *requestPhaseStatsRepo) GetUserDashboardStats(context.Context, int64) (*usagestats.UserDashboardStats, error) {
	return &usagestats.UserDashboardStats{TotalRequests: 10}, nil
}

func (r *requestPhaseStatsRepo) GetAPIKeyDashboardStats(context.Context, int64) (*usagestats.UserDashboardStats, error) {
	return &usagestats.UserDashboardStats{TotalRequests: 10}, nil
}

func (r *requestPhaseStatsRepo) ReadUsageRequestPhaseAverages(_ context.Context, filters usagestats.UsageLogFilters) (usagestats.RequestPhaseAverages, error) {
	r.filters, r.reads = filters, r.reads+1
	upload, processing := 2000.0, 6000.0
	return usagestats.RequestPhaseAverages{AverageUserUploadMs: &upload, AverageProcessingMs: &processing, TimingSampleCount: 2}, r.err
}

func TestUsageRequestPhasesEnrichStatsWithTheirAuthorizedScope(t *testing.T) {
	r := &requestPhaseStatsRepo{}
	s := NewUsageService(r, nil, nil, nil)
	filters := usagestats.UsageLogFilters{UserID: 4, APIKeyID: 5, Model: "public", GroupID: 6}
	stats, err := s.GetStatsWithFilters(context.Background(), filters)
	require.NoError(t, err)
	require.Equal(t, int64(4), r.filters.UserID)
	require.Zero(t, r.filters.APIKeyID)
	require.Empty(t, r.filters.Model)
	require.Equal(t, 24*time.Hour, r.filters.EndTime.Sub(*r.filters.StartTime))
	require.EqualValues(t, 10, stats.TotalRequests)
	require.Equal(t, 7.0, stats.TotalActualCost)
	require.Equal(t, 2000.0, *stats.AverageUserUploadMs)
	require.Equal(t, 6000.0, *stats.AverageProcessingMs)
	require.EqualValues(t, 2, stats.TimingSampleCount)

	user, err := s.GetUserDashboardStats(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, int64(7), r.filters.UserID)
	require.Zero(t, r.filters.APIKeyID)
	require.EqualValues(t, 2, user.TimingSampleCount)
	key, err := s.GetAPIKeyDashboardStats(context.Background(), 8)
	require.NoError(t, err)
	require.Equal(t, int64(8), r.filters.APIKeyID)
	require.Zero(t, r.filters.UserID)
	require.EqualValues(t, 2, key.TimingSampleCount)

	global, err := s.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{})
	require.NoError(t, err)
	require.Equal(t, 4, r.reads)
	require.Zero(t, r.filters.UserID)
	require.Zero(t, r.filters.APIKeyID)
	require.Equal(t, 24*time.Hour, r.filters.EndTime.Sub(*r.filters.StartTime))
	require.Equal(t, 2000.0, *global.AverageUserUploadMs)
	require.Equal(t, 6000.0, *global.AverageProcessingMs)
}

func TestUsageRequestPhasesDiagnosticFailurePreservesBillingStats(t *testing.T) {
	r := &requestPhaseStatsRepo{err: errors.New("timing unavailable")}
	s := NewUsageService(r, nil, nil, nil)
	stats, err := s.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{UserID: 4})
	require.NoError(t, err)
	require.EqualValues(t, 10, stats.TotalRequests)
	require.Equal(t, 7.0, stats.TotalActualCost)
	require.Nil(t, stats.AverageUserUploadMs)
	require.Nil(t, stats.AverageProcessingMs)
	require.Zero(t, stats.TimingSampleCount)
}
