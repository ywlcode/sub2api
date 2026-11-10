package relay

import (
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestTimingsWindowSharesThreeExactAverages(t *testing.T) {
	var timings Timings
	if s := timings.Snapshot(); s.Samples != 0 || s.Upload.AverageMS != nil || s.Preparation.AverageMS != nil || s.FirstToken.AverageMS != nil {
		t.Fatal("empty window reports measured zero")
	}
	count := TimingWindowSize*2 + 37
	for i := range count {
		d := time.Duration(i) * time.Millisecond
		timings.record(d, 2*d, 3*d)
	}
	s := timings.Snapshot()
	expected := float64(2*count-TimingWindowSize-1) / 2
	if s.Samples != TimingWindowSize || *s.Upload.AverageMS != expected || *s.Preparation.AverageMS != 2*expected || *s.FirstToken.AverageMS != 3*expected {
		t.Fatalf("incorrect rolling averages: %+v, upload=%v", s, *s.Upload.AverageMS)
	}
	if unsafe.Sizeof(timings.window) != 48000 {
		t.Fatalf("window stores more than three durations: %d bytes", unsafe.Sizeof(timings.window))
	}
}

func TestTimingsConcurrentSnapshotsAndUpdates(t *testing.T) {
	var timings Timings
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 400 {
				timings.begin()
				timings.record(time.Millisecond, 2*time.Millisecond, 3*time.Millisecond)
				_ = timings.Snapshot()
				timings.finish(false)
			}
		})
	}
	workers.Wait()
	s := timings.Snapshot()
	if s.Completed != 3200 || s.Active != 0 || s.Samples != TimingWindowSize || *s.Upload.AverageMS != 1 || *s.Preparation.AverageMS != 2 || *s.FirstToken.AverageMS != 3 {
		t.Fatalf("concurrent stats inconsistent: %+v", s)
	}
}

func TestFailedOrTokenlessRequestsDoNotBiasWindow(t *testing.T) {
	var timings Timings
	timings.begin()
	timings.finish(true)
	s := timings.Snapshot()
	if s.Completed != 1 || s.Errors != 1 || s.Active != 0 || s.Samples != 0 || s.FirstToken.AverageMS != nil {
		t.Fatalf("failure counted as zero-latency token: %+v", s)
	}
}
