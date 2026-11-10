package service

import "time"

const subscriptionDayDuration = 24 * time.Hour

type UserSubscription struct {
	ID      int64
	UserID  int64
	GroupID int64

	StartsAt  time.Time
	ExpiresAt time.Time
	Status    string

	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time

	DailyUsageUSD   float64
	WeeklyUsageUSD  float64
	MonthlyUsageUSD float64

	AssignedBy *int64
	AssignedAt time.Time
	Notes      string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time

	User           *User
	Group          *Group
	AssignedByUser *User
}

func (s *UserSubscription) IsActive() bool {
	return s.Status == SubscriptionStatusActive && time.Now().Before(s.ExpiresAt)
}

func (s *UserSubscription) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

func (s *UserSubscription) DaysRemaining() int {
	return s.daysRemainingAt(time.Now())
}

func (s *UserSubscription) daysRemainingAt(now time.Time) int {
	remaining := s.ExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}

	days := int(remaining / subscriptionDayDuration)
	if remaining%subscriptionDayDuration != 0 {
		days++
	}
	return days
}

func (s *UserSubscription) IsWindowActivated() bool {
	return s.DailyWindowStart != nil || s.WeeklyWindowStart != nil || s.MonthlyWindowStart != nil
}

func (s *UserSubscription) NeedsDailyReset() bool {
	return s.NeedsDailyResetAt(time.Now())
}

func (s *UserSubscription) NeedsDailyResetAt(now time.Time) bool {
	_, ok := s.automaticDailyWindowStartAt(now)
	return ok
}

func (s *UserSubscription) NeedsWeeklyReset() bool {
	return s.NeedsWeeklyResetAt(time.Now())
}

func (s *UserSubscription) NeedsWeeklyResetAt(now time.Time) bool {
	_, ok := s.automaticWindowStartAt(s.WeeklyWindowStart, subscriptionWeekStart, now)
	return ok
}

func (s *UserSubscription) NeedsMonthlyReset() bool {
	return s.NeedsMonthlyResetAt(time.Now())
}

func (s *UserSubscription) NeedsMonthlyResetAt(now time.Time) bool {
	_, ok := s.automaticWindowStartAt(s.MonthlyWindowStart, subscriptionMonthStart, now)
	return ok
}

func (s *UserSubscription) canAutomaticallyResetDailyAt(now time.Time) bool {
	return s.NeedsDailyResetAt(now)
}

func (s *UserSubscription) automaticDailyWindowStartAt(now time.Time) (time.Time, bool) {
	return s.automaticWindowStartAt(s.DailyWindowStart, subscriptionDayStart, now)
}

func (s *UserSubscription) canAutomaticallyResetWeeklyAt(now time.Time) bool {
	return s.NeedsWeeklyResetAt(now)
}

func (s *UserSubscription) canAutomaticallyResetMonthlyAt(now time.Time) bool {
	return s.NeedsMonthlyResetAt(now)
}

// Advance only when a Beijing calendar boundary has passed. Existing mid-period
// anchors retain their usage until that boundary; late events never rewind them.
// Expiration caps advancement so an expired term cannot receive a new window.
func (s *UserSubscription) automaticWindowStartAt(previous *time.Time, startOfWindow func(time.Time) time.Time, now time.Time) (time.Time, bool) {
	if previous == nil {
		return time.Time{}, false
	}
	if !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt) {
		now = s.ExpiresAt.Add(-time.Nanosecond)
	}
	current := startOfWindow(now)
	if !current.After(startOfWindow(*previous)) {
		return time.Time{}, false
	}
	return current, true
}

func (s *UserSubscription) DailyResetTime() *time.Time {
	if s.DailyWindowStart == nil {
		return nil
	}
	t := subscriptionDayStart(*s.DailyWindowStart).AddDate(0, 0, 1)
	return &t
}

func (s *UserSubscription) WeeklyResetTime() *time.Time {
	if s.WeeklyWindowStart == nil {
		return nil
	}
	t := subscriptionWeekStart(*s.WeeklyWindowStart).AddDate(0, 0, 7)
	return &t
}

func (s *UserSubscription) MonthlyResetTime() *time.Time {
	if s.MonthlyWindowStart == nil {
		return nil
	}
	t := subscriptionMonthStart(*s.MonthlyWindowStart).AddDate(0, 1, 0)
	return &t
}

func (s *UserSubscription) CheckDailyLimit(group *Group, additionalCost float64) bool {
	if !group.HasDailyLimit() {
		return true
	}
	return s.DailyUsageUSD+additionalCost <= *group.DailyLimitUSD
}

func (s *UserSubscription) CheckWeeklyLimit(group *Group, additionalCost float64) bool {
	if !group.HasWeeklyLimit() {
		return true
	}
	return s.WeeklyUsageUSD+additionalCost <= *group.WeeklyLimitUSD
}

func (s *UserSubscription) CheckMonthlyLimit(group *Group, additionalCost float64) bool {
	if !group.HasMonthlyLimit() {
		return true
	}
	return s.MonthlyUsageUSD+additionalCost <= *group.MonthlyLimitUSD
}

func (s *UserSubscription) CheckAllLimits(group *Group, additionalCost float64) (daily, weekly, monthly bool) {
	daily = s.CheckDailyLimit(group, additionalCost)
	weekly = s.CheckWeeklyLimit(group, additionalCost)
	monthly = s.CheckMonthlyLimit(group, additionalCost)
	return
}
