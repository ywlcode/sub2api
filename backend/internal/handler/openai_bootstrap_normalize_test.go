package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func bootstrapDifferentialSeeds() [][]byte {
	auto := `{"type":"function_call_output","namespace":"codex_app","name":"automation_update","output":"<heartbeat><automation_id>wiki</automation_id></heartbeat>"}`
	delegation := `{"type":"function_call_output","namespace":"codex_tui","name":"create_thread","output":"` + delegationEnvelope + `"}`
	history := `{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"done"}`
	seeds := [][]byte{nil, []byte(`null`), []byte(`{`), []byte(`{"input":"ordinary"}`), []byte(`{"input":[{"role":"user","content":"function_call_output"}]}`)}
	for _, items := range []string{auto, delegation, auto + "," + delegation, delegation + "," + auto, history + "," + auto, history + "," + delegation, `{"type":"item_reference","id":"anchor"},` + delegation} {
		for _, prefix := range []string{``, `"previous_response_id":"resp_1",`, `"previous_response_id":null,`, `"large_number":9007199254740993,`, `"ambiguous":1,"ambiguous":2,`} {
			raw := []byte(`{` + prefix + `"input":[` + items + `]}`)
			seeds = append(seeds, raw, bytes.ReplaceAll(raw, []byte(`"input"`), []byte(`"\u0069nput"`)), bytes.ReplaceAll(raw, []byte(`"type"`), []byte(`"\u0074ype"`)))
		}
	}
	seeds = append(seeds, []byte(`{"input":[],"input":[`+auto+`]}`), []byte(`{"input":[`+auto+`]} {}`), []byte(`{"input":[`+auto+`],"huge":1e999}`))
	return seeds
}

func checkBootstrapDifferential(t *testing.T, body []byte) {
	t.Helper()
	original := bytes.Clone(body)
	want, a := normalizeCodexAutomationBootstrap(body)
	want, d := normalizeCodexDelegationBootstrap(want)
	got, gotA, gotD := normalizeCodexBootstraps(body)
	require.Equal(t, string(want), string(got))
	require.Equal(t, a, gotA)
	require.Equal(t, d, gotD)
	require.Equal(t, original, body, "input must remain immutable")
	again, a2, d2 := normalizeCodexBootstraps(got)
	require.Equal(t, string(got), string(again))
	require.False(t, a2 || d2)
}

func TestNormalizeCodexBootstrapsMatchesSequential(t *testing.T) {
	for i, body := range bootstrapDifferentialSeeds() {
		t.Run(fmt.Sprint(i), func(t *testing.T) { checkBootstrapDifferential(t, body) })
	}
}

func FuzzNormalizeCodexBootstraps(f *testing.F) {
	for _, body := range bootstrapDifferentialSeeds() {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<20 {
			t.Skip()
		}
		checkBootstrapDifferential(t, body)
	})
}

func BenchmarkCodexBootstraps(b *testing.B) {
	for _, size := range []int{1, 5, 20} {
		text, _ := json.Marshal(strings.Repeat("x", size<<20))
		body := append([]byte(`{"model":"gpt-6","input":[{"role":"user","content":`), text...)
		body = append(body, []byte(`}]}`)...)
		for _, mode := range []string{"before", "after"} {
			b.Run(fmt.Sprintf("%dMB/%s", size, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					if mode == "before" {
						out, _ := normalizeCodexAutomationBootstrap(body)
						normalizeCodexDelegationBootstrap(out)
					} else {
						normalizeCodexBootstraps(body)
					}
				}
			})
		}
	}
}
