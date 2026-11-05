package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsTokenTodayUsesUserCalendarAndAlignment(t *testing.T) {
	for _, tt := range []struct{ instant, zone, start string }{
		{"2026-11-05T16:24:16.123Z", "Asia/Shanghai", "2026-11-05T16:00:00Z"},
		{"2026-11-05T00:00:10Z", "America/Los_Angeles", "2026-11-04T08:00:00Z"},
		{"2026-03-08T10:01:12Z", "America/Los_Angeles", "2026-03-08T08:00:00Z"},
		{"2026-11-01T10:01:12Z", "America/Los_Angeles", "2026-11-01T07:00:00Z"},
		{"2026-11-05T16:00:00.123Z", "Asia/Shanghai", "2026-11-05T16:00:00Z"},
	} {
		t.Run(tt.instant+tt.zone, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339Nano, tt.instant)
			require.NoError(t, err)
			start, end, err := opsOpenAITokenStatsRange("today", tt.zone, now)
			require.NoError(t, err)
			require.Equal(t, tt.start, start.Format(time.RFC3339))
			require.False(t, end.Before(start))
			require.Equal(t, now.Truncate(30*time.Second), end)
		})
	}
}

func TestOpsPresetsAlignButExplicitRangesStayPrecise(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?timezone=Asia%2FShanghai", nil)
	filter, err := parseOpsOpenAITokenStatsFilter(c)
	require.NoError(t, err)
	require.Equal(t, "today", filter.TimeRange)
	require.Equal(t, int64(0), filter.EndTime.UnixNano()%int64(30*time.Second))
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?time_range=1h", nil)
	start, end, err := parseOpsTimeRange(c, "1h")
	require.NoError(t, err)
	require.Equal(t, time.Hour, end.Sub(start))
	require.Equal(t, int64(0), end.UnixNano()%int64(30*time.Second))
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?start_time=2026-11-05T00:00:12.123Z&end_time=2026-11-05T01:00:13.456Z", nil)
	start, end, err = parseOpsTimeRange(c, "1h")
	require.NoError(t, err)
	require.Equal(t, "2026-11-05T00:00:12.123Z", start.Format(time.RFC3339Nano))
	require.Equal(t, "2026-11-05T01:00:13.456Z", end.Format(time.RFC3339Nano))
}
