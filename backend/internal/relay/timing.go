package relay

import (
	"sort"
	"sync"
	"time"
)

const timingCapacity = 4096

// Timing stores transport timing only, never prompts, credentials or response text.
type Timing struct {
	At                  time.Time `json:"at"`
	RequestID           string    `json:"request_id"`
	UpstreamRequestID   string    `json:"upstream_request_id,omitempty"`
	Path                string    `json:"path"`
	Status              int       `json:"status"`
	Gzip                bool      `json:"gzip"`
	BodyBytes           int64     `json:"body_bytes"`
	ResponseBytes       int64     `json:"response_bytes"`
	UploadMS            float64   `json:"user_upload_ms"`
	CompatibilityMS     float64   `json:"compatibility_ms"`
	ProcessingMS        float64   `json:"processing_ms"`
	UpstreamConnectMS   *float64  `json:"upstream_connection_ms,omitempty"`
	UpstreamWriteMS     *float64  `json:"upstream_request_write_ms,omitempty"`
	UpstreamFirstByteMS *float64  `json:"upstream_first_byte_ms,omitempty"`
	TotalMS             float64   `json:"total_ms"`
	Error               string    `json:"error,omitempty"`
}

type Timings struct {
	mu        sync.Mutex
	started   time.Time
	active    int
	completed uint64
	errors    uint64
	next      int
	records   []Timing
}

func (t *Timings) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.IsZero() {
		t.started = time.Now().UTC()
	}
	t.active++
}

func (t *Timings) finish(record Timing) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	t.completed++
	if record.Status >= 400 || record.Error != "" {
		t.errors++
	}
	if len(t.records) < timingCapacity {
		t.records = append(t.records, record)
	} else {
		t.records[t.next] = record
	}
	t.next = (t.next + 1) % timingCapacity
}

type DurationStats struct {
	AverageMS float64 `json:"average_ms"`
	P50MS     float64 `json:"p50_ms"`
	P95MS     float64 `json:"p95_ms"`
}

type TimingSnapshot struct {
	Since      time.Time     `json:"since"`
	Active     int           `json:"active"`
	Completed  uint64        `json:"completed"`
	Errors     uint64        `json:"errors"`
	Samples    int           `json:"samples"`
	Scope      string        `json:"scope"`
	Upload     DurationStats `json:"user_upload"`
	Processing DurationStats `json:"processing"`
	Total      DurationStats `json:"total"`
	Recent     []Timing      `json:"recent"`
}

func (t *Timings) Snapshot() TimingSnapshot {
	t.mu.Lock()
	s := TimingSnapshot{Since: t.started, Active: t.active, Completed: t.completed, Errors: t.errors, Samples: len(t.records), Scope: "duration statistics cover the last 4096 completed requests; counters are since startup"}
	records := append([]Timing(nil), t.records...)
	next := t.next
	t.mu.Unlock()
	if len(records) == timingCapacity {
		records = append(records[next:], records[:next]...)
	}
	u, p, total := make([]float64, len(records)), make([]float64, len(records)), make([]float64, len(records))
	for i, r := range records {
		u[i], p[i], total[i] = r.UploadMS, r.ProcessingMS, r.TotalMS
	}
	s.Upload, s.Processing, s.Total = summarize(u), summarize(p), summarize(total)
	start := max(0, len(records)-20)
	s.Recent = records[start:]
	return s
}

func summarize(values []float64) DurationStats {
	if len(values) == 0 {
		return DurationStats{}
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	sort.Float64s(values)
	return DurationStats{AverageMS: sum / float64(len(values)), P50MS: values[(len(values)-1)/2], P95MS: values[(len(values)*95+99)/100-1]}
}
