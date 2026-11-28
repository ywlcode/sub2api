package promptcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func anchorEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Options{MaxEntries: 256, MinPrefixTokens: 16, PrefixTokensStep: 8, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestShadowAnchorCapacityInsidePrefixBudget(t *testing.T) {
	e, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, s, _ := e.CacheCapacities()
	anchors, opaque := e.AnchorCapacities()
	if e.Options().MaxEntries != 1000000 || a != 250000 || s != 650000 || anchors != 100000 || opaque != 10000 || a+s+anchors != 1000000 {
		t.Fatalf("unexpected capacity: account=%d logical=%d anchors=%d opaque=%d", a, s, anchors, opaque)
	}
}

func TestShadowMeasuredAnchorSurvivesDensePrefixEviction(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"private-cipher"}]}`)
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(90000, 0, now)
	for _, tenant := range []string{"other-1", "other-2"} {
		scope := testScope()
		scope.Tenant = tenant
		e.Begin(scope, testBody(strings.Repeat(tenant, 500)), byteEncoder, now).CommitUsage(10000, 0, now)
	}
	q := []byte(`{"input":[{"type":"compaction","encrypted_content":"private-cipher"},{"role":"user","content":"next"}]}`)
	a := e.Begin(testScope(), q, byteEncoder, now.Add(time.Second))
	defer a.Release()
	if a.MatchedPrefixTokens != 0 || a.SharedPrefixTokens != 0 || a.AnchorStatus != "hit" || a.OpaqueKnownBlocks != 1 {
		t.Fatalf("dense eviction fixture or protected lookup failed: %+v", a)
	}
	if got, _ := a.Compare(90020); got != 90000 {
		t.Fatalf("protected prediction=%d", got)
	}
}

func TestShadowOpaqueDiagnosticsDoNotInventNewCipherHits(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := []byte(`{"instructions":"old","input":[{"type":"compaction","encrypted_content":"cipher-one"}]}`)
	a := e.Begin(testScope(), body, byteEncoder, now)
	if a.OpaqueKnownBlocks != 0 {
		t.Fatal("first ciphertext claimed previously known")
	}
	frozen := e.Begin(testScope(), body, byteEncoder, now)
	a.CommitUsage(90000, 0, now)
	if frozen.OpaqueKnownBlocks != 0 {
		t.Fatal("future write changed frozen diagnostic")
	}
	frozen.Release()
	changedContext := bytes.ReplaceAll(body, []byte(`"old"`), []byte(`"new"`))
	b := e.Begin(testScope(), changedContext, byteEncoder, now.Add(time.Second))
	defer b.Release()
	if b.OpaqueKnownBlocks != 1 || b.AnchorStatus != "not_found" {
		t.Fatal("seen cipher with changed prefix not distinguished")
	}
	if got, _ := b.Compare(90000); got != 0 {
		t.Fatal("cipher identity alone fabricated token length")
	}
	c := e.Begin(testScope(), bytes.ReplaceAll(body, []byte("cipher-one"), []byte("cipher-two")), byteEncoder, now.Add(time.Second))
	defer c.Release()
	if c.OpaqueKnownBlocks != 0 {
		t.Fatal("changed ciphertext reused opaque history")
	}
	scope := testScope()
	scope.Tenant = "different-user"
	d := e.Begin(scope, body, byteEncoder, now.Add(time.Second))
	defer d.Release()
	if d.OpaqueKnownBlocks != 0 || d.MatchedInputTokens != 0 {
		t.Fatal("opaque history crossed users")
	}
}

func TestShadowOpaqueOutputSourceAndMeasuredContinuation(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	a := e.Begin(testScope(), []byte(`{"input":[{"role":"user","content":"hello"}]}`), byteEncoder, now)
	output := []byte(`[{"type":"reasoning","id":"generated","encrypted_content":"output-cipher"},{"type":"message","role":"assistant","content":"answer"}]`)
	if !a.ObserveOutput(output) {
		t.Fatal("output not observed")
	}
	reserved := e.retainedPoints
	if !a.ObserveOutput(output) || reserved != e.retainedPoints {
		t.Fatal("duplicate output leaked reservation")
	}
	a.CommitUsage(2000, 500, now)
	q := []byte(`{"input":[{"role":"user","content":"hello"},{"type":"reasoning","id":"new-id","encrypted_content":"output-cipher"},{"type":"message","role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`)
	b := e.Begin(testScope(), q, byteEncoder, now.Add(time.Second))
	defer b.Release()
	if b.OpaqueKnownBlocks != 1 || b.OpaqueOutputKnownBlocks != 1 || b.AnchorStatus != "hit" {
		t.Fatalf("output source/anchor missing: %+v", b)
	}
	if got, _ := b.Compare(2510); got < 2500 {
		t.Fatalf("continuation prediction=%d", got)
	}
}

func TestShadowProtectedAnchorExpiryAndPersistence(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"private-persistent-cipher"}]}`)
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(90000, 0, now)
	p := filepath.Join(t.TempDir(), "cache.bin")
	if err := e.Save(p, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-persistent-cipher")) {
		t.Fatal("ciphertext persisted")
	}
	second := anchorEngine(t)
	if err := second.Load(p, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	a := second.Begin(testScope(), body, byteEncoder, now.Add(time.Second))
	defer a.Release()
	if a.OpaqueKnownBlocks != 1 || a.AnchorStatus != "hit" {
		t.Fatal("protected history not restored")
	}
	b := e.Begin(testScope(), body, byteEncoder, now.Add(2*time.Minute))
	defer b.Release()
	if b.MatchedInputTokens != 0 || b.AnchorStatus != "expired" || b.OpaqueKnownBlocks != 0 {
		t.Fatal("expired protected state reused")
	}
}

func TestShadowProtectedAnchorsUpgradeLegacySnapshot(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"old-cipher"}]}`)
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(90000, 0, now)
	data := append([]byte(legacyStateMagic), e.secret[:]...)
	data = binary.BigEndian.AppendUint64(data, uint64(now.UnixNano()))
	for _, cache := range []*prefixCache{&e.prefixCache, &e.shared, &e.warm} {
		data = binary.BigEndian.AppendUint64(data, uint64(len(cache.entries)))
		for elem := cache.lru.Front(); elem != nil; elem = elem.Next() {
			item, ok := elem.Value.(entry)
			if !ok {
				t.Fatal("invalid legacy fixture entry")
			}
			data = append(data, item.key[:]...)
			data = binary.BigEndian.AppendUint64(data, uint64(item.written.UnixNano()))
			var expiry uint64
			if !item.expires.IsZero() {
				expiry = uint64(item.expires.UnixNano())
			}
			data = binary.BigEndian.AppendUint64(data, expiry)
			data = binary.BigEndian.AppendUint64(data, uint64(item.inputTokens))
		}
	}
	sum := sha256.Sum256(data)
	data = append(data, sum[:]...)
	p := filepath.Join(t.TempDir(), "cache.bin")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	next := anchorEngine(t)
	if err := next.Load(p, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(next.anchors.entries) == 0 {
		t.Fatal("legacy measured anchors not migrated")
	}
	a := next.Begin(testScope(), body, byteEncoder, now.Add(time.Second))
	defer a.Release()
	if a.AnchorStatus != "hit" || a.OpaqueKnownBlocks != 0 {
		t.Fatal("legacy migration invented opaque identities or lost anchor")
	}
}

func TestShadowFailedOpaqueAttemptDoesNotWarmHistory(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"failed-cipher"}]}`)
	a := e.Begin(testScope(), body, byteEncoder, now)
	if !a.ObserveOutput([]byte(`[{"type":"reasoning","encrypted_content":"failed-output"}]`)) {
		t.Fatal("fixture output unavailable")
	}
	a.Release()
	if e.retainedPoints != 0 || len(e.opaqueSeen.entries) != 0 || len(e.anchors.entries) != 0 {
		t.Fatal("failed attempt retained/warmed opaque state")
	}
	b := e.Begin(testScope(), body, byteEncoder, now.Add(time.Second))
	defer b.Release()
	if b.OpaqueKnownBlocks != 0 || b.MatchedInputTokens != 0 {
		t.Fatal("failed ciphertext seen as successful history")
	}
}

func TestShadowSuccessfulContinuationRenewsProtectedAnchor(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	first := []byte(`{"input":[{"type":"compaction","encrypted_content":"retained-cipher"}]}`)
	e.Begin(testScope(), first, byteEncoder, now).CommitUsage(90000, 0, now)
	next := []byte(`{"input":[{"type":"compaction","encrypted_content":"retained-cipher"},{"role":"user","content":"next"}]}`)
	a := e.Begin(testScope(), next, byteEncoder, now.Add(50*time.Second))
	a.CommitUsage(90020, 0, now.Add(50*time.Second))
	for _, tenant := range []string{"other-1", "other-2"} {
		scope := testScope()
		scope.Tenant = tenant
		e.Begin(scope, testBody(strings.Repeat(tenant, 500)), byteEncoder, now.Add(50*time.Second)).CommitUsage(10000, 0, now.Add(50*time.Second))
	}
	other := bytes.ReplaceAll(next, []byte(`"next"`), []byte(`"different"`))
	b := e.Begin(testScope(), other, byteEncoder, now.Add(70*time.Second))
	defer b.Release()
	if b.MatchedPrefixTokens != 0 || b.SharedPrefixTokens != 0 {
		t.Fatal("fixture did not evict ordinary indices")
	}
	if got, _ := b.Compare(90020); got != 90000 || b.AnchorStatus != "hit" {
		t.Fatalf("successful continuation failed to renew protected anchor: got=%d status=%s", got, b.AnchorStatus)
	}
	c := e.Begin(testScope(), other, byteEncoder, now.Add(111*time.Second))
	defer c.Release()
	if got, _ := c.Compare(90020); got != 0 {
		t.Fatal("read-only lookup renewed expired anchor")
	}
}
