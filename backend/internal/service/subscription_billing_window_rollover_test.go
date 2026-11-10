package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubscriptionBillingWindowsUseBeijingAndDoNotRewind(t *testing.T) {
	start := time.Date(2026, 1, 31, 15, 30, 0, 0, time.UTC)
	now := time.Date(2026, 2, 1, 16, 0, 0, 0, time.UTC)
	s := UserSubscription{StartsAt: start, ExpiresAt: start.AddDate(0, 1, 0), DailyWindowStart: &start, WeeklyWindowStart: &start, MonthlyWindowStart: &start}
	got := s.BillingWindowStarts(now)
	want := []string{"2026-02-02T00:00:00+08:00", "2026-02-02T00:00:00+08:00", "2026-02-01T00:00:00+08:00"}
	for i := range got {
		require.Equal(t, want[i], got[i].Format(time.RFC3339))
	}
	s.DailyWindowStart, s.WeeklyWindowStart, s.MonthlyWindowStart = &got[0], &got[1], &got[2]
	require.Equal(t, got, s.BillingWindowStarts(start), "late billing must not rewind a newer window")
	s.DailyWindowStart, s.WeeklyWindowStart, s.MonthlyWindowStart = nil, nil, nil
	require.Equal(t, got, s.BillingWindowStarts(now))
}
