package promptcache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShadowDefaultRetentionCoversLongPause(t *testing.T) {
	e, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	body := testBody(strings.Repeat("long cached context ", 100))
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(3000, 0, now)
	a := e.Begin(testScope(), body, byteEncoder, now.Add(31*time.Minute))
	defer a.Release()
	if got, _ := a.Compare(3000); got != 3000 {
		t.Fatalf("31-minute pause prediction=%d", got)
	}
	if a := e.Begin(testScope(), body, byteEncoder, now.Add(61*time.Minute)); a.MatchedPrefixTokens != 0 {
		t.Fatal("one-hour expiry bypassed")
	}
}

func TestShadowOpaqueReuseAndMeasuredInputAnchor(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	first := []byte(`{"input":[{"type":"compaction","id":"item1","encrypted_content":"PRIVATE-CIPHERTEXT"}]}`)
	a := e.Begin(testScope(), first, byteEncoder, now)
	if a.SkipReason != "" || a.OpaqueBlocks != 1 {
		t.Fatalf("opaque coverage: %s count=%d", a.SkipReason, a.OpaqueBlocks)
	}
	a.CommitUsage(90000, 0, now)
	next := []byte(`{"input":[{"type":"compaction","id":"item2","status":"completed","encrypted_content":"PRIVATE-CIPHERTEXT"},{"role":"user","content":"new question"}]}`)
	b := e.Begin(testScope(), next, byteEncoder, now.Add(time.Second))
	defer b.Release()
	if got, _ := b.Compare(90020); got < 90000 {
		t.Fatalf("measured opaque-prefix anchor lost: %d", got)
	}
	changed := bytes.ReplaceAll(next, []byte("PRIVATE-CIPHERTEXT"), []byte("DIFFERENT-CIPHERTEXT"))
	c := e.Begin(testScope(), changed, byteEncoder, now.Add(time.Second))
	defer c.Release()
	if c.MatchedInputTokens != 0 {
		t.Fatal("changed opaque bytes reused a measured anchor")
	}
}

func TestShadowToolSchemaPropertyNamesAreNotOpaqueContent(t *testing.T) {
	e := testEngine(t)
	body := []byte(`{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"signature":{"type":"string"},"encrypted_content":{"type":"string"}}}}],"input":"hello"}`)
	a := e.Begin(testScope(), body, byteEncoder, time.Now())
	defer a.Release()
	if a.SkipReason != "" || a.OpaqueBlocks != 0 {
		t.Fatalf("tool schema misclassified: %s", a.SkipReason)
	}
}

func TestShadowLogicalCacheSurvivesUpstreamSwitch(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	body := testBody(strings.Repeat("logical context ", 40))
	e.Begin(scope, body, byteEncoder, now).CommitUsage(1000, 0, now)
	scope.AccountID++
	scope.RoutingFingerprint = "different-upstream-session"
	a := e.Begin(scope, body, byteEncoder, now.Add(time.Second))
	defer a.Release()
	if a.MatchedPrefixTokens != 0 {
		t.Fatal("account diagnostic unexpectedly shared")
	}
	if got, _ := a.Compare(1000); got != 1000 {
		t.Fatalf("logical-cache switch prediction=%d", got)
	}
	scope.Tenant = "another-user"
	b := e.Begin(scope, body, byteEncoder, now.Add(time.Second))
	defer b.Release()
	if got, _ := b.Compare(1000); got != 0 {
		t.Fatal("logical cache crossed users")
	}
}

func TestShadowCompletedOutputBecomesNextInputPrefix(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	a := e.Begin(testScope(), []byte(`{"input":[{"role":"user","content":"hello"}]}`), byteEncoder, now)
	if !a.ObserveOutput([]byte(`[{"type":"message","id":"generated-id","status":"completed","role":"assistant","content":[{"type":"output_text","text":"world","annotations":[]}]}]`)) {
		t.Fatal("output not observed")
	}
	a.CommitUsage(2000, 500, now)
	b := e.Begin(testScope(), []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"output_text","text":"world"}]},{"role":"user","content":"next"}]}`), byteEncoder, now.Add(time.Second))
	defer b.Release()
	if got, _ := b.Compare(2510); got < 2500 {
		t.Fatalf("completed output anchor lost: %d", got)
	}
}

func TestShadowPersistentFingerprintsAreBoundedPrivateAndExpire(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	body := testBody(strings.Repeat("PRIVATE-PROMPT ", 40))
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(5000, 0, now)
	path := filepath.Join(t.TempDir(), "cache.bin")
	if err := e.Save(path, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("PRIVATE-PROMPT")) {
		t.Fatal("state contains prompt")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
	next := testEngine(t)
	id := next.InstanceID()
	if err := next.Load(path, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if next.InstanceID() != id || id == e.InstanceID() {
		t.Fatal("restart reused process identity")
	}
	a := next.Begin(testScope(), body, byteEncoder, now.Add(time.Second))
	defer a.Release()
	if got, _ := a.Compare(5000); got != 5000 {
		t.Fatalf("restored prefix=%d", got)
	}
	expired := testEngine(t)
	if err := expired.Load(path, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	b := expired.Begin(testScope(), body, byteEncoder, now.Add(2*time.Minute))
	defer b.Release()
	if got, _ := b.Compare(5000); got != 0 {
		t.Fatal("expired persisted prefix restored")
	}
	if err := os.WriteFile(path, []byte("bad snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := next.Load(path, now); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}
