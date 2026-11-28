package promptcache

import "testing"

func TestShadowPredictionUsesUpstreamInputUnits(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		matched, estimated, input int
		want                      int
	}{
		{"observed high-cache request", 167168, 167825, 143644, 143081},
		{"partial prefix", 500, 2000, 1000, 250},
		{"larger upstream template", 500, 1000, 2000, 1000},
		{"full prefix", 2048, 2048, 1700, 1700},
		{"cold remains zero", 0, 2048, 1700, 0},
		{"no artificial minimum", 1, 10000, 1000, 0},
		{"missing input", 1000, 2000, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Attempt{MatchedPrefixTokens: tc.matched, EstimatedInputTokens: tc.estimated}
			predicted, raw := a.Compare(tc.input)
			if predicted != tc.want {
				t.Fatalf("prediction=%d want=%d (matched=%d estimated=%d upstream_input=%d)", predicted, tc.want, tc.matched, tc.estimated, tc.input)
			}
			if tc.input > 0 && raw != tc.matched {
				t.Fatalf("raw prefix diagnostic=%d want=%d", raw, tc.matched)
			}
		})
	}
}

func TestShadowPredictionIntegerPrecision(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	a := &Attempt{MatchedPrefixTokens: maxInt - 1, EstimatedInputTokens: maxInt}
	got, raw := a.Compare(maxInt)
	if got != maxInt-1 || raw != maxInt-1 {
		t.Fatalf("integer prediction lost precision: predicted=%d raw=%d want=%d", got, raw, maxInt-1)
	}
}
