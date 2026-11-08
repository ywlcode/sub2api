package service

import (
	"strings"
	"testing"
)

func BenchmarkVolcengineNormalizationPipeline(b *testing.B) {
	account := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{DeepSeekVolcengineCompatibilityCredential: true}}
	for _, size := range []int{2 << 20, 20 << 20} {
		body := []byte(`{"model":"deepseek-flash","stream":true,"input":[{"type":"message","role":"user","content":"` + strings.Repeat("abcdefgh", size/8) + `"}]}`)
		b.Run(normalizationBenchmarkSize(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				normalized, err := normalizeVolcengineRequestBody(account, body, APIProtocolResponses)
				if err != nil {
					b.Fatal(err)
				}
				normalized = normalizeDeepSeekResponsesRequestBody(account, normalized)
				if _, err := normalizeVolcengineRequestBody(account, normalized, APIProtocolResponses); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func normalizationBenchmarkSize(size int) string {
	if size == 2<<20 {
		return "2MiB"
	}
	return "20MiB"
}

// A single adapter, including its no-change check, for comparison with the
// three-stage pipeline. Neither benchmark measures end-to-end request latency.
func BenchmarkVolcengineNormalizationSinglePass(b *testing.B) {
	account := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{DeepSeekVolcengineCompatibilityCredential: true}}
	for _, size := range []int{2 << 20, 20 << 20} {
		body := []byte(`{"model":"deepseek-flash","stream":true,"input":[{"type":"message","role":"user","content":"` + strings.Repeat("abcdefgh", size/8) + `"}]}`)
		b.Run(normalizationBenchmarkSize(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := normalizeVolcengineRequestBody(account, body, APIProtocolResponses); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
