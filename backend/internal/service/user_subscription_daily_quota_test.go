package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type dailyResetTrackingUserSubRepo struct {
	userSubRepoNoop

	resetDailyCalled bool
}

func (r *dailyResetTrackingUserSubRepo) ResetDailyUsage(context.Context, int64, *time.Time, time.Time) error {
	r.resetDailyCalled = true
	return nil
}

func TestAssignOrExtendSubscription_ExpiredDailyCardStartsNewCalendarQuota(t *testing.T) {
	groupRepo := &subscriptionGroupRepoStub{
		group: &Group{ID: 1, SubscriptionType: SubscriptionTypeSubscription},
	}
	subRepo := newSubscriptionUserSubRepoStub()
	oldStart := time.Now().AddDate(0, 0, -3)
	oldWindowStart := startOfDay(oldStart)
	subRepo.seed(&UserSubscription{
		ID:                 100,
		UserID:             200,
		GroupID:            1,
		StartsAt:           oldStart,
		ExpiresAt:          oldStart.AddDate(0, 0, 1),
		Status:             SubscriptionStatusExpired,
		DailyWindowStart:   &oldWindowStart,
		WeeklyWindowStart:  &oldWindowStart,
		MonthlyWindowStart: &oldWindowStart,
		DailyUsageUSD:      10,
		WeeklyUsageUSD:     20,
		MonthlyUsageUSD:    30,
		Notes:              "old",
	})
	svc := NewSubscriptionService(groupRepo, subRepo, nil, nil, nil)

	renewed, reused, err := svc.AssignOrExtendSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       200,
		GroupID:      1,
		ValidityDays: 1,
		Notes:        "new",
	})

	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, SubscriptionStatusActive, renewed.Status)
	require.True(t, renewed.StartsAt.After(oldStart), "重新购买过期订阅时应重置当前周期 StartsAt")
	require.False(t, renewed.ExpiresAt.After(renewed.StartsAt.AddDate(0, 0, 1)))
	require.NotNil(t, renewed.DailyWindowStart)
	require.Equal(t, subscriptionDayStart(renewed.StartsAt), *renewed.DailyWindowStart, "续期后日窗口应锚定当天 0 点")
	require.Equal(t, 0.0, renewed.DailyUsageUSD)
	require.Equal(t, 0.0, renewed.WeeklyUsageUSD)
	require.Equal(t, 0.0, renewed.MonthlyUsageUSD)
	require.Equal(t, "old\nnew", renewed.Notes)
}

func TestAssignOrExtendSubscription_ExpiredSubscriptionAppendsMatchingNotes(t *testing.T) {
	groupRepo := &subscriptionGroupRepoStub{
		group: &Group{ID: 1, SubscriptionType: SubscriptionTypeSubscription},
	}
	subRepo := newSubscriptionUserSubRepoStub()
	oldStart := time.Now().AddDate(0, 0, -3)
	subRepo.seed(&UserSubscription{
		ID:        101,
		UserID:    201,
		GroupID:   1,
		StartsAt:  oldStart,
		ExpiresAt: oldStart.AddDate(0, 0, 1),
		Status:    SubscriptionStatusExpired,
		Notes:     "same",
	})
	svc := NewSubscriptionService(groupRepo, subRepo, nil, nil, nil)

	renewed, reused, err := svc.AssignOrExtendSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       201,
		GroupID:      1,
		ValidityDays: 1,
		Notes:        "same",
	})

	require.NoError(t, err)
	require.True(t, reused)
	require.Equal(t, "same\nsame", renewed.Notes)
}

func TestUserSubscriptionNeedsDailyReset_DailyCardUsesBeijingMidnight(t *testing.T) {
	start := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	dailyWindowStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	sub := &UserSubscription{
		StartsAt:         start,
		ExpiresAt:        start.Add(24 * time.Hour),
		DailyWindowStart: &dailyWindowStart,
		DailyUsageUSD:    10,
	}

	require.True(t, sub.NeedsDailyResetAt(dailyWindowStart.Add(25*time.Hour)))
}

func TestUserSubscriptionNeedsDailyReset_MultiDaySubscriptionStillRefreshes(t *testing.T) {
	start := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	dailyWindowStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	sub := &UserSubscription{
		StartsAt:         start,
		ExpiresAt:        start.AddDate(0, 0, 2),
		DailyWindowStart: &dailyWindowStart,
	}

	require.True(t, sub.NeedsDailyResetAt(dailyWindowStart.Add(24*time.Hour)), "多日订阅也在北京时间午夜刷新")
}

func TestUserSubscriptionDailyResetTime_DailyCardReturnsBeijingMidnight(t *testing.T) {
	start := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	dailyWindowStart := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	expiresAt := start.Add(24 * time.Hour)
	sub := &UserSubscription{
		StartsAt:         start,
		ExpiresAt:        expiresAt,
		DailyWindowStart: &dailyWindowStart,
	}

	resetAt := sub.DailyResetTime()
	require.NotNil(t, resetAt)
	require.Equal(t, subscriptionDayStart(dailyWindowStart).AddDate(0, 0, 1), *resetAt)
}

func TestCheckAndResetWindows_DailyCardResetsDailyUsage(t *testing.T) {
	now := time.Now()
	startsAt := now.Add(-23 * time.Hour)
	dailyWindowStart := now.Add(-25 * time.Hour)
	repo := &dailyResetTrackingUserSubRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	sub := &UserSubscription{
		ID:               1,
		UserID:           10,
		GroupID:          20,
		StartsAt:         startsAt,
		ExpiresAt:        startsAt.Add(24 * time.Hour),
		DailyUsageUSD:    10,
		DailyWindowStart: &dailyWindowStart,
	}

	err := svc.CheckAndResetWindows(context.Background(), sub)

	require.NoError(t, err)
	require.True(t, repo.resetDailyCalled)
	require.Zero(t, sub.DailyUsageUSD)
}

func TestCheckAndResetWindows_MultiDaySubscriptionStillResetsDailyUsage(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	startsAt := now.Add(-48 * time.Hour)
	dailyWindowStart := now.Add(-25 * time.Hour)
	repo := &dailyResetTrackingUserSubRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := &UserSubscription{
		ID:               1,
		UserID:           10,
		GroupID:          20,
		StartsAt:         startsAt,
		ExpiresAt:        startsAt.AddDate(0, 0, 4),
		DailyUsageUSD:    10,
		DailyWindowStart: &dailyWindowStart,
	}

	err := svc.CheckAndResetWindows(context.Background(), sub)

	require.NoError(t, err)
	require.True(t, repo.resetDailyCalled, "多日订阅仍应重置过期 daily window")
	require.Equal(t, 0.0, sub.DailyUsageUSD)
}

func TestValidateAndCheckLimits_DailyCardUsesNewCalendarDay(t *testing.T) {
	start := time.Now().Add(-23 * time.Hour)
	dailyWindowStart := time.Now().Add(-25 * time.Hour)
	dailyLimit := 10.0
	sub := &UserSubscription{
		Status:           SubscriptionStatusActive,
		StartsAt:         start,
		ExpiresAt:        start.Add(24 * time.Hour),
		DailyWindowStart: &dailyWindowStart,
		DailyUsageUSD:    dailyLimit + 0.01,
	}
	group := &Group{
		SubscriptionType: SubscriptionTypeSubscription,
		DailyLimitUSD:    &dailyLimit,
	}
	svc := NewSubscriptionService(groupRepoNoop{}, userSubRepoNoop{}, nil, nil, nil)

	needsMaintenance, err := svc.ValidateAndCheckLimits(sub, group)

	require.True(t, needsMaintenance)
	require.NoError(t, err)
	require.Zero(t, sub.DailyUsageUSD)
}
