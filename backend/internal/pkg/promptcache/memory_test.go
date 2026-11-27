package promptcache

import (
	"encoding/binary"
	"runtime"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Run explicitly with -bench BenchmarkShadowRetainedMemory -benchtime=1x,
// without -race. These measurements are retained Go heap, not a process RSS cap.
func BenchmarkShadowRetainedMemory(b *testing.B) {
	measure := func(b *testing.B, build func() any) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		value := build()
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained_B")
		runtime.KeepAlive(value)
	}
	b.Run("index_100000", func(b *testing.B) {
		measure(b, func() any {
			e, err := New(Options{})
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now()
			for i := 0; i < e.limit; i++ {
				var key [32]byte
				binary.LittleEndian.PutUint64(key[:8], uint64(i))
				e.put(key, now)
			}
			for i := 0; i < e.shared.limit; i++ {
				var key [32]byte
				binary.LittleEndian.PutUint64(key[:8], uint64(i))
				e.shared.put(key, now, e.options.TTL)
			}
			return e
		})
	})
	b.Run("warm_state_10000", func(b *testing.B) {
		measure(b, func() any {
			e, err := New(Options{})
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now()
			for i := 0; i < e.warm.limit; i++ {
				var key [32]byte
				binary.LittleEndian.PutUint64(key[:8], uint64(i))
				e.warm.put(key, now, 0)
			}
			return e
		})
	})
	b.Run("inflight_65536_points", func(b *testing.B) {
		measure(b, func() any {
			var pending [4][]point
			for i := range pending {
				pending[i] = make([]point, 16384)
			}
			return pending
		})
	})
	b.Run("log_queue_1024_typical_events", func(b *testing.B) {
		measure(b, func() any {
			queue := make(chan queuedLogEvent, 1024)
			for i := 0; i < cap(queue); i++ {
				fields := make([]zap.Field, 60)
				for j := range fields {
					fields[j] = zap.Int("input_tokens", 100000)
				}
				queue <- queuedLogEvent{time.Now(), fields}
			}
			return queue
		})
	})
}
