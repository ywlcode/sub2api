package service

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBatchDeepSeekReasoningMatchesSequential(t *testing.T) {
	account := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	for _, msg := range []string{
		`{"role":"assistant","content":"hi"}`,
		`{"role":"assistant","reasoning_content":null,"content":"hi"}`,
		`{"role":"assistant","reasoning_content":"","content":"hi"}`,
		`{"role":"assistant","reasoning_content":"real reasoning","tool_calls":[{"id":"c1"}]}`,
		`{"role":"assistant","reasoning_content":0}`,
		`{"role":"assistant","reasoning_content":false}`,
		`{"role":"assistant","reasoning_content":"","reasoning_content":"real"}`,
		`{"role":"assistant","role":"user"}`,
		`{"\u0072ole":"assistant","\u0072easoning_content":""}`,
		`{"role":"user","content":"assistant reasoning_content"}`,
		`null`, `"assistant"`, `[]`,
	} {
		t.Run(msg, func(t *testing.T) {
			body := []byte(" \n" + `{"messages":[` + msg + `,{"role":"assistant","content":"second"}],"tools":[{"name":"x"}]}` + " \n")
			original := bytes.Clone(body)
			want := ensureDeepSeekChatReasoningPlaceholdersSequential(body)
			got := ensureDeepSeekChatReasoningPlaceholders(account, body)
			require.Equal(t, string(want), string(got))
			require.Equal(t, original, body)
			require.Equal(t, string(got), string(ensureDeepSeekChatReasoningPlaceholders(account, got)))
		})
	}
	for _, raw := range []string{`{`, `{"messages":[]}`, `{"messages":null}`, `{"messages":[{"role":"assistant"}],"messages":[{"role":"user"}]}`, `{"messages":[{"role":"assistant"}]`, `{"messages":[{"role":"assistant"}]} {}`} {
		body := []byte(raw)
		require.Equal(t, string(ensureDeepSeekChatReasoningPlaceholdersSequential(body)), string(ensureDeepSeekChatReasoningPlaceholders(account, body)))
	}
}

func FuzzBatchDeepSeekReasoning(f *testing.F) {
	for _, seed := range []string{`{"messages":[{"role":"assistant"}]}`, `{"messages":[]}`, `{"messages":[{"role":"assistant","reasoning_content":null}]}`, `{"messages":[{"role":"assistant"},{"role":"assistant","reasoning_content":""}]}`, ` {"messages":[{"role":"user","content":"x"},{"role":"assistant","tool_calls":[{"id":"c"}]},{"role":"assistant","reasoning_content":null}]} `} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<20 {
			t.Skip()
		}
		original := bytes.Clone(body)
		want := ensureDeepSeekChatReasoningPlaceholdersSequential(body)
		got := ensureDeepSeekChatReasoningPlaceholders(&Account{Platform: PlatformDeepseek}, body)
		require.Equal(t, string(want), string(got))
		require.Equal(t, original, body)
	})
}

func BenchmarkDeepSeekReasoningBatch(b *testing.B) {
	account := &Account{Platform: PlatformDeepseek}
	for _, size := range []int{1, 5, 20} {
		for _, count := range []int{1, 50} {
			body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", size<<20) + `"}` + strings.Repeat(`,{"role":"assistant","content":"reply","tool_calls":[{"id":"call_1"}]}`, count) + `]}`)
			for _, mode := range []string{"before", "after"} {
				b.Run(fmt.Sprintf("%dMB/%d_messages/%s", size, count, mode), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(body)))
					for b.Loop() {
						if mode == "before" {
							ensureDeepSeekChatReasoningPlaceholdersSequential(body)
						} else {
							ensureDeepSeekChatReasoningPlaceholders(account, body)
						}
					}
				})
			}
		}
	}
}

// Fixed worker counts avoid multiplying concurrency by the machine's GOMAXPROCS.
// Report p95 separately from ns/op, which measures aggregate throughput here.
func BenchmarkDeepSeekReasoningConcurrent(b *testing.B) {
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}` + strings.Repeat(`,{"role":"assistant","content":"reply"}`, 50) + `]}`)
	account := &Account{Platform: PlatformDeepseek}
	for _, workers := range []int{1, 8, 25} {
		for _, mode := range []string{"before", "after"} {
			b.Run(fmt.Sprintf("workers_%d/%s", workers, mode), func(b *testing.B) {
				samples := make([]int64, b.N)
				var next atomic.Int64
				var wg sync.WaitGroup
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for range workers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for {
							i := next.Add(1) - 1
							if i >= int64(len(samples)) {
								return
							}
							start := time.Now()
							if mode == "before" {
								ensureDeepSeekChatReasoningPlaceholdersSequential(body)
							} else {
								ensureDeepSeekChatReasoningPlaceholders(account, body)
							}
							samples[i] = time.Since(start).Nanoseconds()
						}
					}()
				}
				wg.Wait()
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				if len(samples) > 0 {
					b.ReportMetric(float64(samples[(len(samples)-1)*95/100])/1e6, "p95_ms")
				}
			})
		}
	}
}
