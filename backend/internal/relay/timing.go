package relay

import (
	"sync"
	"time"
)

const TimingWindowSize = 2000

// Each entry contains only three durations. No IDs, bodies or request metadata
// are retained. All three sums share the same window of valid first-token samples.
type timingSample struct {
	upload      time.Duration
	preparation time.Duration
	firstToken  time.Duration
}

type Timings struct {
	mu        sync.Mutex
	started   time.Time
	active    int
	completed uint64
	errors    uint64
	next      int
	samples   int
	sums      timingSample
	window    [TimingWindowSize]timingSample
}

func (t *Timings) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.IsZero() {
		t.started = time.Now().UTC()
	}
	t.active++
}

func (t *Timings) finish(failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	t.completed++
	if failed {
		t.errors++
	}
}

// record runs once on the first semantic token, even if the response keeps
// streaming. Missing tokens never become artificial zero-latency samples.
func (t *Timings) record(upload, preparation, firstToken time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.window[t.next]
	if t.samples < TimingWindowSize {
		t.samples++
	}
	sample := timingSample{upload: upload, preparation: preparation, firstToken: firstToken}
	t.sums.upload += sample.upload - old.upload
	t.sums.preparation += sample.preparation - old.preparation
	t.sums.firstToken += sample.firstToken - old.firstToken
	t.window[t.next] = sample
	t.next = (t.next + 1) % TimingWindowSize
}

type DurationStats struct {
	AverageMS *float64 `json:"average_ms"`
}

type TimingSnapshot struct {
	Since       time.Time     `json:"since"`
	Active      int           `json:"active"`
	Completed   uint64        `json:"completed"`
	Errors      uint64        `json:"errors"`
	Samples     int           `json:"samples"`
	WindowSize  int           `json:"window_size"`
	Upload      DurationStats `json:"user_upload"`
	Preparation DurationStats `json:"local_preparation"`
	FirstToken  DurationStats `json:"upstream_first_token"`
}

func (t *Timings) Snapshot() TimingSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := TimingSnapshot{Since: t.started, Active: t.active, Completed: t.completed, Errors: t.errors, Samples: t.samples, WindowSize: TimingWindowSize}
	if t.samples != 0 {
		average := func(d time.Duration) *float64 { v := milliseconds(d) / float64(t.samples); return &v }
		s.Upload.AverageMS = average(t.sums.upload)
		s.Preparation.AverageMS = average(t.sums.preparation)
		s.FirstToken.AverageMS = average(t.sums.firstToken)
	}
	return s
}
