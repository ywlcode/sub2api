//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type activateWindowUserSubRepo struct {
	userSubRepoNoop
	dailyStart time.Time
	windowTime time.Time
}

func (r *activateWindowUserSubRepo) ActivateWindows(_ context.Context, _ int64, dailyStart, windowTime time.Time) error {
	r.dailyStart, r.windowTime = dailyStart, windowTime
	return nil
}

type monthlyResetUserSubRepo struct {
	userSubRepoNoop
	resetCalled bool
	expected    *time.Time
	resetAt     time.Time
}

func (r *monthlyResetUserSubRepo) ResetMonthlyUsage(_ context.Context, _ int64, expected *time.Time, resetAt time.Time) error {
	r.resetCalled, r.expected, r.resetAt = true, expected, resetAt
	return nil
}
func calendarTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}
func TestSubscriptionCalendarBoundariesUseBeijing(t *testing.T) {
	for _, tt := range []struct {
		name, anchor, boundary string
		start                  func(time.Time) time.Time
	}{
		{"daily", "2026-07-01T23:45:00+08:00", "2026-07-02T00:00:00+08:00", subscriptionDayStart},
		{"weekly", "2026-07-01T23:45:00+08:00", "2026-07-06T00:00:00+08:00", subscriptionWeekStart},
		{"sunday", "2026-07-05T23:59:59+08:00", "2026-07-06T00:00:00+08:00", subscriptionWeekStart},
		{"monthly", "2026-01-31T23:45:00+08:00", "2026-02-01T00:00:00+08:00", subscriptionMonthStart},
		{"february", "2026-02-01T10:00:00+08:00", "2026-03-01T00:00:00+08:00", subscriptionMonthStart},
		{"leap_february", "2028-02-01T10:00:00+08:00", "2028-03-01T00:00:00+08:00", subscriptionMonthStart},
		{"year_end", "2026-12-31T23:45:00+08:00", "2027-01-01T00:00:00+08:00", subscriptionMonthStart},
	} {
		t.Run(tt.name, func(t *testing.T) {
			anchor, boundary := calendarTime(t, tt.anchor), calendarTime(t, tt.boundary)
			sub := &UserSubscription{StartsAt: anchor, ExpiresAt: boundary.AddDate(0, 1, 0)}
			_, reset := sub.automaticWindowStartAt(&anchor, tt.start, boundary.Add(-time.Nanosecond))
			require.False(t, reset, "retain usage until midnight in Beijing")
			next, reset := sub.automaticWindowStartAt(&anchor, tt.start, boundary.UTC())
			require.True(t, reset)
			require.Equal(t, tt.boundary, next.Format(time.RFC3339))
			_, reset = sub.automaticWindowStartAt(&next, tt.start, boundary.Add(time.Hour))
			require.False(t, reset, "do not reset an already advanced window twice")
		})
	}
}
func TestSubscriptionCalendarCapsAdvancementAtExpiry(t *testing.T) {
	anchor := calendarTime(t, "2026-01-15T17:00:00+08:00")
	expires := calendarTime(t, "2026-02-01T00:00:00+08:00")
	sub := &UserSubscription{StartsAt: anchor, ExpiresAt: expires}
	for _, now := range []time.Time{expires, expires.AddDate(0, 4, 0)} {
		_, reset := sub.automaticWindowStartAt(&anchor, subscriptionMonthStart, now)
		require.False(t, reset)
	}
	sub.ExpiresAt = calendarTime(t, "2026-02-10T17:00:00+08:00")
	next, reset := sub.automaticWindowStartAt(&anchor, subscriptionMonthStart, expires.AddDate(0, 4, 0))
	require.True(t, reset)
	require.Equal(t, "2026-02-01T00:00:00+08:00", next.Format(time.RFC3339))
}
func TestSubscriptionCalendarPreservesCurrentAndFutureManualUsage(t *testing.T) {
	anchor := calendarTime(t, "2026-02-20T17:00:00+08:00")
	sub := &UserSubscription{ExpiresAt: anchor.AddDate(1, 0, 0)}
	for _, now := range []time.Time{anchor.Add(-time.Hour), anchor.AddDate(0, 0, -1), anchor.AddDate(0, 0, 1)} {
		_, reset := sub.automaticWindowStartAt(&anchor, subscriptionMonthStart, now)
		require.False(t, reset)
	}
	next, reset := sub.automaticWindowStartAt(&anchor, subscriptionMonthStart, calendarTime(t, "2026-05-19T16:00:00+08:00"))
	require.True(t, reset)
	require.Equal(t, "2026-05-01T00:00:00+08:00", next.Format(time.RFC3339))
}
func TestDelayedFirstUseUsesCurrentBeijingCalendar(t *testing.T) {
	repo := &activateWindowUserSubRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	now := calendarTime(t, "2026-07-10T23:30:00Z")
	svc.now = func() time.Time { return now }
	sub := &UserSubscription{ID: 1, StartsAt: now.AddDate(0, 0, -10), ExpiresAt: now.AddDate(0, 1, 0)}
	require.NoError(t, svc.CheckAndActivateWindow(context.Background(), sub))
	require.Equal(t, "2026-07-11T00:00:00+08:00", repo.dailyStart.Format(time.RFC3339))
	require.Equal(t, now, repo.windowTime)
}
func TestCheckAndResetWindowsCalendarMonthUsesConditionalReset(t *testing.T) {
	anchor := calendarTime(t, "2026-01-31T23:30:00+08:00")
	now := calendarTime(t, "2026-02-01T00:00:00+08:00")
	repo := &monthlyResetUserSubRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := &UserSubscription{ID: 2, StartsAt: anchor, ExpiresAt: anchor.AddDate(0, 1, 0), MonthlyWindowStart: &anchor, MonthlyUsageUSD: 12}
	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.True(t, repo.resetCalled)
	require.Equal(t, &anchor, repo.expected)
	require.True(t, now.Equal(repo.resetAt))
	require.Zero(t, sub.MonthlyUsageUSD)
}
func TestNormalizeCalendarMonthUpdatesUsageAndDisplayedWindow(t *testing.T) {
	anchor := calendarTime(t, "2026-01-31T23:30:00+08:00")
	now := calendarTime(t, "2026-02-01T00:00:00+08:00")
	subs := []UserSubscription{{StartsAt: anchor, ExpiresAt: anchor.AddDate(0, 1, 0), MonthlyWindowStart: &anchor, MonthlyUsageUSD: 12}}
	normalizeExpiredWindowsAt(subs, now)
	require.Zero(t, subs[0].MonthlyUsageUSD)
	require.True(t, now.Equal(*subs[0].MonthlyWindowStart))
	require.Equal(t, "2026-03-01T00:00:00+08:00", subs[0].MonthlyResetTime().Format(time.RFC3339))
}
func TestValidateAndCheckLimitsUsesBeijingMonthAndRejectsExactExpiry(t *testing.T) {
	anchor := calendarTime(t, "2026-01-31T23:30:00+08:00")
	now := calendarTime(t, "2026-02-01T00:00:00+08:00")
	limit := 10.0
	sub := &UserSubscription{Status: SubscriptionStatusActive, StartsAt: anchor, ExpiresAt: now.AddDate(0, 0, 1), MonthlyWindowStart: &anchor, MonthlyUsageUSD: 12}
	svc := NewSubscriptionService(groupRepoNoop{}, userSubRepoNoop{}, nil, nil, nil)
	svc.now = func() time.Time { return now }
	maintenance, err := svc.ValidateAndCheckLimits(sub, &Group{MonthlyLimitUSD: &limit})
	require.NoError(t, err)
	require.True(t, maintenance)
	require.Zero(t, sub.MonthlyUsageUSD)
	sub.ExpiresAt = now
	maintenance, err = svc.ValidateAndCheckLimits(sub, &Group{})
	require.ErrorIs(t, err, ErrSubscriptionExpired)
	require.False(t, maintenance)
}
