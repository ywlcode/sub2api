package promptcache

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func byteEncoder(s string) ([]uint, error) {
	ids := make([]uint, len(s))
	for i := range s {
		ids[i] = uint(s[i])
	}
	return ids, nil
}
func testEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Options{TTL: time.Minute, MinPrefixTokens: 16, PrefixTokensStep: 8, MaxEntries: 2000})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func testBody(text string) []byte {
	body, _ := json.Marshal(map[string]any{"model": "test", "input": []any{map[string]any{"role": "user", "content": text}}})
	return body
}
func testScope() Scope {
	return Scope{Tenant: "key:7/group:2", Platform: "openai", Model: "gpt-test", AccountID: 1}
}

func TestShadowPrefixAppendAndMutation(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	text := strings.Repeat("prefix ", 100)
	first := e.Begin(scope, testBody(text), byteEncoder, now)
	if first.Decision != "cold" || first.MatchedPrefixTokens != 0 {
		t.Fatalf("cold=%+v", first)
	}
	if !first.Commit(now) {
		t.Fatal("not committed")
	}
	next := e.Begin(scope, testBody(text+" new content"), byteEncoder, now.Add(time.Second))
	if next.Decision != "hit" || next.MatchedPrefixTokens < 600 || next.MatchedPrefixTokens >= next.EstimatedInputTokens {
		t.Fatalf("append: hit=%d total=%d decision=%s", next.MatchedPrefixTokens, next.EstimatedInputTokens, next.Decision)
	}
	changed := e.Begin(scope, testBody("changed "+text), byteEncoder, now.Add(time.Second))
	if changed.MatchedPrefixTokens >= next.MatchedPrefixTokens {
		t.Fatal("non-prefix text was reused")
	}
	if !next.Commit(now) {
		t.Fatal("first commit failed")
	}
	if next.Commit(now) {
		t.Fatal("double commit")
	}
}

func TestShadowIsolationAndSharedCandidate(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	body := testBody(strings.Repeat("context ", 70))
	e.Begin(scope, body, byteEncoder, now).Commit(now)
	for _, kind := range []string{"tenant", "platform", "model", "account"} {
		t.Run(kind, func(t *testing.T) {
			other := scope
			switch kind {
			case "tenant":
				other.Tenant = "different"
			case "platform":
				other.Platform = "deepseek"
			case "model":
				other.Model = "different"
			case "account":
				other.AccountID = 2
			}
			got := e.Begin(other, body, byteEncoder, now)
			if got.MatchedPrefixTokens != 0 {
				t.Fatal("cross-scope hit")
			}
			if kind == "account" && got.SharedPrefixTokens == 0 {
				t.Fatal("shared account diagnostic lost")
			}
			if kind != "account" && got.SharedPrefixTokens != 0 {
				t.Fatal("shared diagnostic crossed tenant/model/platform")
			}
		})
	}
}

func TestShadowTTLRefreshAndConcurrentCompletion(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	body := testBody(strings.Repeat("x ", 100))
	scope := testScope()
	first := e.Begin(scope, body, byteEncoder, now)
	inflight := e.Begin(scope, body, byteEncoder, now)
	first.Commit(now)
	if inflight.MatchedPrefixTokens != 0 {
		t.Fatal("future completion changed frozen prediction")
	}
	warm := e.Begin(scope, body, byteEncoder, now.Add(50*time.Second))
	if warm.MatchedPrefixTokens == 0 {
		t.Fatal("missing warm hit")
	}
	warm.Commit(now.Add(50 * time.Second))
	if e.Begin(scope, body, byteEncoder, now.Add(70*time.Second)).MatchedPrefixTokens == 0 {
		t.Fatal("TTL did not refresh")
	}
	expired := e.Begin(scope, body, byteEncoder, now.Add(111*time.Second))
	if expired.MatchedPrefixTokens != 0 || !expired.Expired {
		t.Fatal("expired cache hit or missing expiry marker")
	}
}

func TestShadowCanonicalContentAndRequestControls(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	body := []byte(`{"model":"test","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"input":"` + strings.Repeat("long context ", 30) + `","stream":false,"metadata":{"timestamp":1}}`)
	a := e.Begin(scope, body, byteEncoder, now)
	a.Commit(now)
	var value map[string]any
	_ = json.Unmarshal(body, &value)
	value["stream"] = true
	value["metadata"] = map[string]any{"timestamp": 2}
	other, _ := json.MarshalIndent(value, "", "  ")
	b := e.Begin(scope, other, byteEncoder, now)
	if b.MatchedPrefixTokens == 0 || a.PrefixFingerprint != b.PrefixFingerprint {
		t.Fatal("JSON formatting / non-prompt metadata changed fingerprint")
	}
	value["tools"] = []any{map[string]any{"type": "function", "name": "changed"}}
	other, _ = json.Marshal(value)
	if e.Begin(scope, other, byteEncoder, now).MatchedPrefixTokens >= b.MatchedPrefixTokens {
		t.Fatal("tool definition change ignored")
	}
	value["reasoning"] = map[string]any{"effort": "high"}
	other, _ = json.Marshal(value)
	if e.Begin(scope, other, byteEncoder, now).MatchedPrefixTokens >= b.MatchedPrefixTokens {
		t.Fatal("prompt controls ignored")
	}
}

func TestShadowOpaqueAndUnsupportedNeverWarm(t *testing.T) {
	for _, body := range []string{
		`{"model":"x","previous_response_id":"resp_x","input":"hello"}`,
		`{"input":[{"type":"item_reference","id":"item_x"}]}`,
		`{"input":[{"type":"input_image","image_url":"data:secret"}]}`,
		`{"prompt":{"id":"saved"},"input":"hello"}`,
		`{"model":"x"}`, `not json`,
	} {
		t.Run(body[:min(25, len(body))], func(t *testing.T) {
			a := testEngine(t).Begin(testScope(), []byte(body), byteEncoder, time.Now())
			if a.SkipReason == "" || a.Commit(time.Now()) {
				t.Fatal("unverifiable input warmed cache")
			}
		})
	}
}

func TestShadowInstructionsAndAccountingBound(t *testing.T) {
	e := testEngine(t)
	scope := testScope()
	scope.Model = "gpt-6.2-astra"
	now := time.Now()
	body := func(instruction string) []byte {
		raw, _ := json.Marshal(map[string]any{"input": strings.Repeat("context ", 100), "instructions": instruction})
		return raw
	}
	a := e.Begin(scope, body("old instructions"), byteEncoder, now)
	a.Commit(now)
	b := e.Begin(scope, body("new instructions"), byteEncoder, now)
	if b.MatchedPrefixTokens >= a.EstimatedInputTokens {
		t.Fatal("instruction change ignored")
	}
	predicted, raw := b.Compare(50)
	if raw != b.MatchedPrefixTokens || predicted > 50 || predicted < 0 {
		t.Fatal("raw diagnostic changed or upstream-unit prediction out of bounds")
	}
	scope.Model = "gpt-6.1-sol"
	e.Begin(scope, body("old instructions"), byteEncoder, now).Commit(now)
	if c := e.Begin(scope, body("new instructions"), byteEncoder, now); c.MatchedPrefixTokens >= a.EstimatedInputTokens {
		t.Fatal("earlier instruction change was ignored")
	}
}

func TestShadowBoundsAndParallelRequests(t *testing.T) {
	e, err := New(Options{MinPrefixTokens: 8, PrefixTokensStep: 8, MaxEntries: 64, MaxBodyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			scope := testScope()
			scope.AccountID = int64(id)
			a := e.Begin(scope, testBody(strings.Repeat("context ", 50)), byteEncoder, time.Now())
			a.Commit(time.Now())
		}(i)
	}
	wg.Wait()
	if len(e.entries) > 64 {
		t.Fatal("entry bound exceeded")
	}
	a := e.Begin(testScope(), make([]byte, 4097), byteEncoder, time.Now())
	if a.SkipReason != "body_limit" {
		t.Fatal("body bound bypassed")
	}
}

func TestShadowDoesNotSeeWritesAfterObservationStarted(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	body := testBody(strings.Repeat("context ", 100))
	started, release := make(chan struct{}), make(chan struct{})
	var barrier sync.Once
	done := make(chan *Attempt, 1)
	go func() {
		done <- e.Begin(scope, body, func(s string) ([]uint, error) {
			barrier.Do(func() { close(started); <-release })
			return byteEncoder(s)
		}, now)
	}()
	<-started
	e.Begin(scope, body, byteEncoder, now.Add(time.Second)).Commit(now.Add(time.Second))
	close(release)
	if observed := <-done; observed.MatchedPrefixTokens != 0 {
		t.Fatal("future cache write affected earlier lookup")
	}
}

func TestShadowPreservesLargeJSONIntegers(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	first := []byte(`{"input":[{"role":"user","content":"` + strings.Repeat("context ", 100) + `","tool_value":9007199254740992}]}`)
	second := []byte(strings.Replace(string(first), "9007199254740992", "9007199254740993", 1))
	a := e.Begin(scope, first, byteEncoder, now)
	a.Commit(now)
	b := e.Begin(scope, second, byteEncoder, now)
	if a.PrefixFingerprint == b.PrefixFingerprint {
		t.Fatal("large JSON integers collapsed through float64")
	}
}

func TestShadowConcurrentRefreshDoesNotHideWarmPrefix(t *testing.T) {
	e := testEngine(t)
	scope := testScope()
	body := testBody(strings.Repeat("context ", 100))
	now := time.Now()
	e.Begin(scope, body, byteEncoder, now).Commit(now)
	started, release := make(chan struct{}), make(chan struct{})
	var barrier sync.Once
	done := make(chan *Attempt, 1)
	go func() {
		done <- e.Begin(scope, body, func(s string) ([]uint, error) {
			barrier.Do(func() { close(started); <-release })
			return byteEncoder(s)
		}, time.Now())
	}()
	<-started
	e.Begin(scope, body, byteEncoder, time.Now()).Commit(time.Now())
	close(release)
	a := <-done
	defer a.Release()
	if a.MatchedPrefixTokens == 0 || a.Decision != "hit" {
		t.Fatal("concurrent refresh hid already-warm prefix")
	}
}

func TestShadowFingerprintReservationBounds(t *testing.T) {
	e, err := New(Options{MinPrefixTokens: 1, PrefixTokensStep: 1, MaxBodyBytes: 100000})
	if err != nil {
		t.Fatal(err)
	}
	body := testBody(strings.Repeat("x", 12000))
	var attempts []*Attempt
	for i := 0; i < 6; i++ {
		a := e.Begin(testScope(), body, byteEncoder, time.Now())
		attempts = append(attempts, a)
	}
	if attempts[5].SkipReason != "observer_budget" {
		t.Fatal("in-flight fingerprint budget not enforced")
	}
	for _, a := range attempts {
		a.Release()
	}
	if e.retainedPoints != 0 {
		t.Fatal("reservations leaked")
	}
	a := e.Begin(testScope(), testBody(strings.Repeat("x", 20000)), byteEncoder, time.Now())
	if a.SkipReason != "fingerprint_limit" {
		t.Fatal("per-attempt bound not enforced")
	}
	b := e.Begin(testScope(), body, byteEncoder, time.Now())
	b.Commit(time.Now())
	if e.retainedPoints != 0 || len(b.points) != 0 {
		t.Fatal("successful commit retained fingerprint budget")
	}
}

func BenchmarkShadowLargePrompt(b *testing.B) {
	e, _ := New(Options{})
	body := testBody(strings.Repeat("context ", 20000))
	scope := testScope()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := e.Begin(scope, body, byteEncoder, time.Now())
		a.Commit(time.Now())
	}
}

func TestShadowReviewEvictionAndNewSessionStayWarm(t *testing.T) {
	e, err := New(Options{MaxEntries: 64, MinPrefixTokens: 16, PrefixTokensStep: 8})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scope := testScope()
	body := testBody(strings.Repeat("context ", 100))
	first := e.Begin(scope, body, byteEncoder, now)
	first.Commit(now)
	var root map[string]any
	_ = json.Unmarshal(body, &root)
	root["prompt_cache_key"] = "new-session"
	nextBody, _ := json.Marshal(root)
	next := e.Begin(scope, nextBody, byteEncoder, now.Add(time.Second))
	defer next.Release()
	if next.Decision != "hit" {
		t.Fatalf("new upstream session must retain the logical cache: %s", next.Decision)
	}
	next = e.Begin(scope, body, byteEncoder, now.Add(2*time.Second))
	defer next.Release()
	if next.Decision == "cold" {
		t.Fatal("fingerprint LRU eviction must not erase warm account/model state")
	}
}

func TestShadowReviewEmptySystemDoesNotChangeProtocol(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("context ", 80) + `"}]}`)
	e.Begin(scope, body, byteEncoder, now).Commit(now)
	var value map[string]any
	_ = json.Unmarshal(body, &value)
	value["system"] = ""
	other, _ := json.Marshal(value)
	next := e.Begin(scope, other, byteEncoder, now.Add(time.Second))
	defer next.Release()
	if next.MatchedPrefixTokens == 0 {
		t.Fatalf("empty optional system changed protocol/layout: %s", next.Protocol)
	}
}

func TestShadowReviewSeparateCachesAndWarmState(t *testing.T) {
	e, err := New(Options{MaxEntries: 64, MinPrefixTokens: 16, PrefixTokensStep: 8})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scope := testScope()
	body := testBody(strings.Repeat("context ", 100))
	a := e.Begin(scope, body, byteEncoder, now)
	last := a.points[len(a.points)-1]
	key := e.key(a.accountNamespace, last.hash)
	a.Commit(now)
	for i := 0; i < 200; i++ {
		var hash [32]byte
		hash[0], hash[1] = byte(i), byte(i/256)
		e.shared.put(hash, now, time.Minute)
	}
	if _, ok := e.lookup(key, now); !ok {
		t.Fatal("shared diagnostics evicted an account prefix")
	}
	for i := 0; i < 200; i++ {
		var hash [32]byte
		hash[0], hash[1] = byte(i), byte(i/256)
		e.put(hash, now)
	}
	next := e.Begin(scope, body, byteEncoder, now.Add(time.Second))
	defer next.Release()
	if next.Decision != "miss" || next.AccountEvictions == 0 || next.SharedEvictions == 0 {
		t.Fatalf("LRU miss hidden as cold: %+v", next)
	}
	if len(e.entries)+len(e.shared.entries) > 64 || len(e.warm.entries) > e.warm.limit {
		t.Fatal("separate cache budgets exceeded")
	}
}

func TestShadowReviewOutboundRoutingAndProtocolIsolation(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	scope := testScope()
	scope.Protocol = ProtocolMessages
	scope.RoutingFingerprint = e.RoutingFingerprint("actual-session-a", "conversation")
	body := []byte(`{"model":"test","messages":[{"role":"user","content":"` + strings.Repeat("context ", 100) + `"}]}`)
	a := e.Begin(scope, body, byteEncoder, now)
	if a.Protocol != ProtocolMessages {
		t.Fatalf("missing system misclassified Messages: %s", a.Protocol)
	}
	a.Commit(now)
	scope.RoutingFingerprint = e.RoutingFingerprint("actual-session-b", "conversation")
	next := e.Begin(scope, body, byteEncoder, now.Add(time.Second))
	defer next.Release()
	if next.MatchedPrefixTokens != 0 || next.SharedPrefixTokens == 0 || next.Decision != "hit" {
		t.Fatal("header-only routing changes must be visible without erasing warmth")
	}
	scope.RoutingFingerprint = e.RoutingFingerprint("actual-session-a", "conversation")
	scope.Protocol = ProtocolChatCompletions
	chat := e.Begin(scope, body, byteEncoder, now.Add(time.Second))
	defer chat.Release()
	if chat.MatchedPrefixTokens != 0 || chat.Protocol != ProtocolChatCompletions {
		t.Fatal("different wire protocols shared an account prefix")
	}
}
