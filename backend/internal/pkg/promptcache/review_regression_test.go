package promptcache

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShadowReviewShortRequestsDoNotOccupyProtectedAnchors(t *testing.T) {
	for _, tc := range []struct{ input, output, want int }{{8, 7, 0}, {16, 0, 2}, {8, 8, 1}} {
		e := anchorEngine(t)
		now := time.Now()
		a := e.Begin(testScope(), testBody(strings.Repeat("context ", 40)), byteEncoder, now)
		if !a.ObserveOutput([]byte(`[{"role":"assistant","content":"answer"}]`)) {
			t.Fatal("output fixture was not indexed")
		}
		a.CommitUsage(tc.input, tc.output, now)
		if len(e.anchors.entries) != tc.want {
			t.Fatalf("input=%d output=%d: protected anchors=%d want=%d", tc.input, tc.output, len(e.anchors.entries), tc.want)
		}
	}
}

func TestShadowReviewOpaquePartialWithoutAnchorIsNotHit(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := []byte(`{"instructions":"` + strings.Repeat("common ", 40) + `","input":[{"type":"compaction","encrypted_content":"first"}]}`)
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(90000, 0, now)
	changed := []byte(strings.Replace(string(body), `"first"`, `"second"`, 1))
	a := e.Begin(testScope(), changed, byteEncoder, now.Add(time.Second))
	defer a.Release()
	got, raw := a.Compare(90000)
	if got != 0 || raw == 0 || a.Decision == "hit" {
		t.Fatalf("partial opaque match mislabelled: predicted=%d raw=%d decision=%s", got, raw, a.Decision)
	}
}

func TestShadowReviewPredictionRespectsConfiguredMinimum(t *testing.T) {
	e := anchorEngine(t)
	now := time.Now()
	body := testBody(strings.Repeat("schema ", 80))
	e.Begin(testScope(), body, byteEncoder, now).CommitUsage(32, 0, now)
	a := e.Begin(testScope(), body, byteEncoder, now.Add(time.Second))
	defer a.Release()
	if got, raw := a.Compare(15); got != 0 || raw == 0 {
		t.Fatalf("below configured minimum: predicted=%d raw=%d", got, raw)
	}
	if got, _ := a.Compare(16); got != 16 {
		t.Fatalf("minimum boundary should allow reuse: %d", got)
	}
}

func TestShadowReviewSnapshotToleratesSmallClockRollback(t *testing.T) {
	for _, offset := range []time.Duration{time.Millisecond, time.Minute, 2 * time.Minute} {
		e := anchorEngine(t)
		now := time.Now()
		written := now.Add(offset)
		body := testBody(strings.Repeat("persistent ", 40))
		e.Begin(testScope(), body, byteEncoder, written).CommitUsage(2048, 0, written)
		path := filepath.Join(t.TempDir(), "cache.bin")
		if err := e.Save(path, now); err != nil {
			t.Fatal(err)
		}
		next := anchorEngine(t)
		err := next.Load(path, now)
		if offset > time.Minute {
			if err == nil {
				t.Fatal("accepted a far-future entry")
			}
			continue
		}
		if err != nil {
			t.Fatalf("rollback=%s: %v", offset, err)
		}
		for _, cache := range []*prefixCache{&next.prefixCache, &next.shared, &next.anchors, &next.warm} {
			for _, elem := range cache.entries {
				item := elem.Value.(entry)
				if item.written.After(now) || !item.expires.IsZero() && item.expires.After(now.Add(next.options.TTL)) {
					t.Fatal("restoration retained future age or extended TTL")
				}
			}
		}
	}
}
