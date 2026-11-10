package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Task 5: 验证 calculateProgress 纯函数行为正确 ---

func newTestSubscriptionService() *SubscriptionService {
	return &SubscriptionService{now: time.Now}
}

func ptrFloat64(v float64) *float64  { return &v }
func ptrTime(t time.Time) *time.Time { return &t }

// Legacy fixtures retain their original timezone to exercise existing anchors.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func TestCalculateProgress_BasicFields(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()

	sub := &UserSubscription{
		ID:        100,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	group := &Group{
		Name: "Premium",
	}

	progress := svc.calculateProgress(sub, group)

	assert.Equal(t, int64(100), progress.ID)
	assert.Equal(t, "Premium", progress.GroupName)
	assert.Equal(t, sub.ExpiresAt, progress.ExpiresAt)
	assert.Equal(t, 30, progress.ExpiresInDays)
	assert.Nil(t, progress.Daily, "无日限额时 Daily 应为 nil")
	assert.Nil(t, progress.Weekly, "无周限额时 Weekly 应为 nil")
	assert.Nil(t, progress.Monthly, "无月限额时 Monthly 应为 nil")
}

func TestCalculateProgress_DailyUsage(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()
	dailyStart := subscriptionDayStart(now)

	sub := &UserSubscription{
		ID:               1,
		ExpiresAt:        now.Add(10 * 24 * time.Hour),
		DailyUsageUSD:    3.0,
		DailyWindowStart: ptrTime(dailyStart),
	}
	group := &Group{
		Name:          "Pro",
		DailyLimitUSD: ptrFloat64(10.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Daily, "有日限额和窗口时 Daily 不应为 nil")
	assert.Equal(t, 10.0, progress.Daily.LimitUSD)
	assert.Equal(t, 3.0, progress.Daily.UsedUSD)
	assert.Equal(t, 7.0, progress.Daily.RemainingUSD)
	assert.Equal(t, 30.0, progress.Daily.Percentage)
	assert.Equal(t, dailyStart, progress.Daily.WindowStart)
}

func TestCalculateProgress_DailyCardUsesBeijingMidnight(t *testing.T) {
	svc := newTestSubscriptionService()
	startsAt := time.Date(2026, 7, 31, 18, 0, 0, 0, subscriptionLocation)
	svc.now = func() time.Time { return startsAt.Add(time.Hour) }
	dailyStart := subscriptionDayStart(startsAt)
	expiresAt := startsAt.Add(24 * time.Hour)

	sub := &UserSubscription{
		ID:               1,
		StartsAt:         startsAt,
		ExpiresAt:        expiresAt,
		DailyUsageUSD:    3.0,
		DailyWindowStart: ptrTime(dailyStart),
	}
	group := &Group{
		Name:          "Daily",
		DailyLimitUSD: ptrFloat64(10.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Daily, "日卡有日限额和窗口时 Daily 不应为 nil")
	assert.Equal(t, dailyStart.AddDate(0, 0, 1), progress.Daily.ResetsAt, "短期订阅也在北京时间午夜刷新")
}

func TestCalculateProgress_WeeklyUsage(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()
	weeklyStart := subscriptionWeekStart(now)

	sub := &UserSubscription{
		ID:                1,
		ExpiresAt:         now.Add(10 * 24 * time.Hour),
		WeeklyUsageUSD:    25.0,
		WeeklyWindowStart: ptrTime(weeklyStart),
	}
	group := &Group{
		Name:           "Pro",
		WeeklyLimitUSD: ptrFloat64(50.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Weekly, "有周限额和窗口时 Weekly 不应为 nil")
	assert.Equal(t, 50.0, progress.Weekly.LimitUSD)
	assert.Equal(t, 25.0, progress.Weekly.UsedUSD)
	assert.Equal(t, 25.0, progress.Weekly.RemainingUSD)
	assert.Equal(t, 50.0, progress.Weekly.Percentage)
}

// 旧版开通日锚点在首次跨周之前保留用量，展示的刷新时间改为北京时间周一。
func TestCalculateProgress_WeeklyResetsAt_LegacyMidnightAnchor(t *testing.T) {
	svc := newTestSubscriptionService()
	startsAt := time.Date(2026, 7, 31, 13, 37, 6, 0, time.FixedZone("UTC+8", 8*3600))
	weeklyStart := subscriptionDayStart(startsAt)
	svc.now = func() time.Time { return startsAt }

	sub := &UserSubscription{
		ID:                1,
		StartsAt:          startsAt,
		ExpiresAt:         startsAt.AddDate(0, 0, 30),
		WeeklyUsageUSD:    2000.18,
		WeeklyWindowStart: ptrTime(weeklyStart),
	}
	group := &Group{
		Name:           "Pro",
		WeeklyLimitUSD: ptrFloat64(2000.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Weekly)
	assert.Equal(t, subscriptionWeekStart(startsAt).AddDate(0, 0, 7), progress.Weekly.ResetsAt)
	assert.Equal(t, sub.WeeklyUsageUSD, progress.Weekly.UsedUSD, "同一周内保留原有用量")
}

func TestCalculateProgress_MonthlyUsage(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()
	monthlyStart := subscriptionMonthStart(now)

	sub := &UserSubscription{
		ID:                 1,
		ExpiresAt:          now.Add(10 * 24 * time.Hour),
		MonthlyUsageUSD:    80.0,
		MonthlyWindowStart: ptrTime(monthlyStart),
	}
	group := &Group{
		Name:            "Enterprise",
		MonthlyLimitUSD: ptrFloat64(100.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Monthly, "有月限额和窗口时 Monthly 不应为 nil")
	assert.Equal(t, 100.0, progress.Monthly.LimitUSD)
	assert.Equal(t, 80.0, progress.Monthly.UsedUSD)
	assert.Equal(t, 20.0, progress.Monthly.RemainingUSD)
	assert.Equal(t, 80.0, progress.Monthly.Percentage)
}

func TestCalculateProgress_OverLimit_ClampedTo100Percent(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()

	sub := &UserSubscription{
		ID:               1,
		ExpiresAt:        now.Add(10 * 24 * time.Hour),
		DailyUsageUSD:    15.0, // 超过限额
		DailyWindowStart: ptrTime(subscriptionDayStart(now)),
	}
	group := &Group{
		Name:          "Pro",
		DailyLimitUSD: ptrFloat64(10.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Daily)
	assert.Equal(t, 100.0, progress.Daily.Percentage, "超额使用应被截断为 100%")
	assert.Equal(t, 0.0, progress.Daily.RemainingUSD, "超额使用时剩余应为 0")
}

func TestCalculateProgress_NoWindowStart_NoProgress(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()

	// 有限额但无窗口起始时间（订阅未激活）
	sub := &UserSubscription{
		ID:             1,
		ExpiresAt:      now.Add(10 * 24 * time.Hour),
		DailyUsageUSD:  0,
		WeeklyUsageUSD: 0,
	}
	group := &Group{
		Name:           "Pro",
		DailyLimitUSD:  ptrFloat64(10.0),
		WeeklyLimitUSD: ptrFloat64(50.0),
	}

	progress := svc.calculateProgress(sub, group)

	assert.Nil(t, progress.Daily, "无 DailyWindowStart 时 Daily 应为 nil")
	assert.Nil(t, progress.Weekly, "无 WeeklyWindowStart 时 Weekly 应为 nil")
}

func TestCalculateProgress_AllLimits(t *testing.T) {
	svc := newTestSubscriptionService()
	now := time.Now()

	sub := &UserSubscription{
		ID:                 1,
		ExpiresAt:          now.Add(10 * 24 * time.Hour),
		DailyUsageUSD:      5.0,
		WeeklyUsageUSD:     20.0,
		MonthlyUsageUSD:    60.0,
		DailyWindowStart:   ptrTime(subscriptionDayStart(now)),
		WeeklyWindowStart:  ptrTime(subscriptionWeekStart(now)),
		MonthlyWindowStart: ptrTime(subscriptionMonthStart(now)),
	}
	group := &Group{
		Name:            "Full",
		DailyLimitUSD:   ptrFloat64(10.0),
		WeeklyLimitUSD:  ptrFloat64(50.0),
		MonthlyLimitUSD: ptrFloat64(100.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Daily)
	require.NotNil(t, progress.Weekly)
	require.NotNil(t, progress.Monthly)

	assert.Equal(t, 50.0, progress.Daily.Percentage)
	assert.Equal(t, 40.0, progress.Weekly.Percentage)
	assert.Equal(t, 60.0, progress.Monthly.Percentage)
}

func TestCalculateProgress_ExpiredSubscription(t *testing.T) {
	svc := newTestSubscriptionService()

	sub := &UserSubscription{
		ID:        1,
		ExpiresAt: time.Now().Add(-24 * time.Hour), // 已过期
	}
	group := &Group{Name: "Expired"}

	progress := svc.calculateProgress(sub, group)

	assert.Equal(t, 0, progress.ExpiresInDays, "过期订阅的剩余天数应为 0")
}

func TestCalculateProgress_ResetsInSeconds_NotNegative(t *testing.T) {
	svc := newTestSubscriptionService()
	// 使用过去的窗口起始时间，使得重置时间已过
	pastStart := time.Now().Add(-48 * time.Hour)

	sub := &UserSubscription{
		ID:               1,
		ExpiresAt:        time.Now().Add(10 * 24 * time.Hour),
		DailyUsageUSD:    1.0,
		DailyWindowStart: ptrTime(pastStart),
	}
	group := &Group{
		Name:          "Test",
		DailyLimitUSD: ptrFloat64(10.0),
	}

	progress := svc.calculateProgress(sub, group)

	require.NotNil(t, progress.Daily)
	assert.GreaterOrEqual(t, progress.Daily.ResetsInSeconds, int64(0),
		"ResetsInSeconds 不应为负数")
}

func TestCalculateProgress_NormalizesAllCalendarWindowsWithoutMutatingSource(t *testing.T) {
	location := time.FixedZone("Beijing", 8*3600)
	now := time.Date(2026, 2, 2, 0, 0, 0, 0, location)
	old := time.Date(2026, 1, 31, 23, 30, 0, 0, location)
	svc := &SubscriptionService{now: func() time.Time { return now.UTC() }}
	sub := &UserSubscription{
		StartsAt: old, ExpiresAt: now.AddDate(0, 1, 0),
		DailyWindowStart: &old, WeeklyWindowStart: &old, MonthlyWindowStart: &old,
		DailyUsageUSD: 10, WeeklyUsageUSD: 20, MonthlyUsageUSD: 30,
	}
	group := &Group{DailyLimitUSD: ptrFloat64(10), WeeklyLimitUSD: ptrFloat64(20), MonthlyLimitUSD: ptrFloat64(30)}
	progress := svc.calculateProgress(sub, group)
	for i, window := range []*UsageWindowProgress{progress.Daily, progress.Weekly, progress.Monthly} {
		require.NotNil(t, window)
		require.Zero(t, window.UsedUSD)
		require.Equal(t, window.LimitUSD, window.RemainingUSD)
		require.Equal(t, []string{"2026-02-03T00:00:00+08:00", "2026-02-09T00:00:00+08:00", "2026-03-01T00:00:00+08:00"}[i], window.ResetsAt.Format(time.RFC3339))
		require.Equal(t, int64(window.ResetsAt.Sub(now).Seconds()), window.ResetsInSeconds)
	}
	require.Equal(t, old, *sub.DailyWindowStart)
	require.Equal(t, old, *sub.WeeklyWindowStart)
	require.Equal(t, old, *sub.MonthlyWindowStart)
	require.Equal(t, 10.0, sub.DailyUsageUSD)
	require.Equal(t, 20.0, sub.WeeklyUsageUSD)
	require.Equal(t, 30.0, sub.MonthlyUsageUSD)
}
