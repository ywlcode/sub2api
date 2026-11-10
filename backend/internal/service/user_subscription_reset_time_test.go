package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubscriptionResetTime_BeijingCalendar(t *testing.T) {
	startsAt := time.Date(2026, 7, 31, 13, 37, 6, 0, subscriptionLocation)
	for _, anchor := range []time.Time{subscriptionDayStart(startsAt), startsAt} {
		sub := &UserSubscription{
			StartsAt: startsAt, ExpiresAt: startsAt.AddDate(0, 0, 60),
			DailyWindowStart: &anchor, WeeklyWindowStart: &anchor, MonthlyWindowStart: &anchor,
		}
		for _, tc := range []struct {
			name          string
			resetAt       *time.Time
			startOfWindow func(time.Time) time.Time
			want          time.Time
		}{
			{"daily", sub.DailyResetTime(), subscriptionDayStart, time.Date(2026, 8, 1, 0, 0, 0, 0, subscriptionLocation)},
			{"weekly", sub.WeeklyResetTime(), subscriptionWeekStart, time.Date(2026, 8, 3, 0, 0, 0, 0, subscriptionLocation)},
			{"monthly", sub.MonthlyResetTime(), subscriptionMonthStart, time.Date(2026, 8, 1, 0, 0, 0, 0, subscriptionLocation)},
		} {
			t.Run(tc.name+"/"+anchor.Format(time.RFC3339), func(t *testing.T) {
				require.NotNil(t, tc.resetAt)
				require.Equal(t, tc.want, *tc.resetAt)
				_, ok := sub.automaticWindowStartAt(&anchor, tc.startOfWindow, tc.want.Add(-time.Nanosecond))
				require.False(t, ok, "the displayed boundary must match the actual rollover")
				next, ok := sub.automaticWindowStartAt(&anchor, tc.startOfWindow, tc.want)
				require.True(t, ok)
				require.Equal(t, tc.want, next)
			})
		}
	}
}

func TestWeeklyResetTime_AdvancedWindowStillUsesMonday(t *testing.T) {
	anchor := time.Date(2026, 8, 7, 13, 37, 6, 0, subscriptionLocation)
	sub := &UserSubscription{WeeklyWindowStart: &anchor}
	require.Equal(t, time.Date(2026, 8, 10, 0, 0, 0, 0, subscriptionLocation), *sub.WeeklyResetTime())
}
