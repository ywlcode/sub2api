package service

import "time"

// Subscription limits always follow Beijing calendar boundaries, independently
// of the server's or client's timezone. Beijing standard time is UTC+08:00.
var subscriptionLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

func subscriptionDayStart(t time.Time) time.Time {
	t = t.In(subscriptionLocation)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, subscriptionLocation)
}

func subscriptionWeekStart(t time.Time) time.Time {
	t = subscriptionDayStart(t)
	daysSinceMonday := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -daysSinceMonday)
}

func subscriptionMonthStart(t time.Time) time.Time {
	t = t.In(subscriptionLocation)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, subscriptionLocation)
}

// SubscriptionWindowStarts returns the day, Monday-based week and calendar
// month starts in Beijing time. Activations and manual resets share this policy.
func SubscriptionWindowStarts(t time.Time) [3]time.Time {
	return [3]time.Time{subscriptionDayStart(t), subscriptionWeekStart(t), subscriptionMonthStart(t)}
}
