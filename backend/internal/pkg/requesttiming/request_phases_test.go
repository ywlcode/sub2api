package requesttiming

import (
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRequestPhasesIncludeFirstByteWaitAndPostUploadWork(t *testing.T) {
	start := time.Now()
	// Application admission takes 100 ms, the first body read blocks for 5 s,
	// then routing/retries/output take 90 s. A one-read body has no interval
	// between body_first_byte and body_received; that must not become 0 upload.
	times := []time.Duration{100 * time.Millisecond, 5100 * time.Millisecond, 95100 * time.Millisecond}
	collector := newCollector(start, 3, func() time.Time {
		next := times[0]
		times = times[1:]
		return start.Add(next)
	})
	buf := make([]byte, 3)
	_, err := collector.WrapBody(io.NopCloser(strings.NewReader("abc"))).Read(buf)
	require.NoError(t, err)
	collector.Finish(200, false)
	collector.WhenFinished(func(s Snapshot) {
		phases := DeriveContentObservation(s).RequestPhases
		require.Equal(t, 5000.0, *phases.UserUploadMS)
		require.Equal(t, 90000.0, *phases.ProcessingMS)
	})
}

func TestRequestPhasesRejectMissingAndInvalidBoundaries(t *testing.T) {
	for name, mutate := range map[string]func(*Snapshot){
		"historical":          func(s *Snapshot) { s.Events = nil },
		"incomplete_body":     func(s *Snapshot) { s.BodyComplete = false },
		"read_error":          func(s *Snapshot) { s.Events["body_read_error"] = 6 },
		"missing_start":       func(s *Snapshot) { delete(s.Events, "body_read_start") },
		"negative_start":      func(s *Snapshot) { s.Events["body_read_start"] = -1 },
		"reversed_upload":     func(s *Snapshot) { s.Events["body_received"] = 0 },
		"reversed_processing": func(s *Snapshot) { s.TotalMS = 5 },
		"nan":                 func(s *Snapshot) { s.TotalMS = math.NaN() },
		"infinite":            func(s *Snapshot) { s.Events["body_received"] = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			s := Snapshot{BodyComplete: true, TotalMS: 10, Events: map[string]float64{"body_read_start": 1, "body_received": 6}}
			mutate(&s)
			phases := DeriveRequestPhases(s)
			require.Nil(t, phases.UserUploadMS)
			require.Nil(t, phases.ProcessingMS)
		})
	}
	zero := DeriveRequestPhases(Snapshot{BodyComplete: true, Events: map[string]float64{"body_read_start": 0, "body_received": 0}})
	require.Equal(t, 0.0, *zero.UserUploadMS)
	require.Equal(t, 0.0, *zero.ProcessingMS)
}
