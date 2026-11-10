package service

import "time"

// BillingWindowStarts computes the same automatic windows used by the normal
// subscription service without mutating counters. Authorities persist these
// starts under their billing row lock. Late events never rewind a newer window.
func (s *UserSubscription) BillingWindowStarts(now time.Time) [3]time.Time {
	starts := SubscriptionWindowStarts(now)
	previous := [3]*time.Time{s.DailyWindowStart, s.WeeklyWindowStart, s.MonthlyWindowStart}
	for i, p := range previous {
		if p != nil {
			starts[i] = *p
		}
	}
	if next, ok := s.automaticDailyWindowStartAt(now); ok {
		starts[0] = next
	}
	if next, ok := s.automaticWindowStartAt(s.WeeklyWindowStart, subscriptionWeekStart, now); ok {
		starts[1] = next
	}
	if next, ok := s.automaticWindowStartAt(s.MonthlyWindowStart, subscriptionMonthStart, now); ok {
		starts[2] = next
	}
	return starts
}
