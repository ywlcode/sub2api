package dto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionDTOIncludesBeijingResetTimes(t *testing.T) {
	// Historical anchors may be unaligned and stored in UTC.
	anchor := time.Date(2026, 2, 28, 15, 30, 0, 0, time.UTC)
	sub := &service.UserSubscription{
		DailyWindowStart: &anchor, WeeklyWindowStart: &anchor, MonthlyWindowStart: &anchor,
	}
	for _, tc := range []struct {
		name string
		dto  any
	}{
		{"user", UserSubscriptionFromService(sub)},
		{"admin", UserSubscriptionFromServiceAdmin(sub)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.dto)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(raw, &got))
			require.Equal(t, "2026-03-01T00:00:00+08:00", got["daily_resets_at"])
			require.Equal(t, "2026-03-02T00:00:00+08:00", got["weekly_resets_at"])
			require.Equal(t, "2026-03-01T00:00:00+08:00", got["monthly_resets_at"])
		})
	}
	empty := UserSubscriptionFromService(&service.UserSubscription{})
	require.Nil(t, empty.DailyResetsAt)
	require.Nil(t, empty.WeeklyResetsAt)
	require.Nil(t, empty.MonthlyResetsAt)
}
