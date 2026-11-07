package handler

import (
	"bytes"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/jsonview"
	"github.com/tidwall/gjson"
)

// normalizeCodexBootstraps shares decoding between the two independent rules.
// Ordinary conversations do not build full maps. The prefilter may accept false
// positives, but the existing duplicate-key and per-rule checks remain decisive.
func normalizeCodexBootstraps(body []byte) ([]byte, bool, bool) {
	if !mayContainCodexBootstrap(body) || !hasUniqueJSONMembers(body) {
		return body, false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		return body, false, false
	}
	automation := normalizeCodexBootstrapObject(request, isCodexAutomationCandidate, false)
	delegation := normalizeCodexBootstrapObject(request, isCodexDelegationCandidate, true)
	if !automation && !delegation {
		return body, false, false
	}
	normalized, err := json.Marshal(request)
	if err != nil {
		return body, false, false
	}
	return normalized, automation, delegation
}

func mayContainCodexBootstrap(body []byte) bool {
	found := false
	// Inspect every top-level input, including duplicate/escaped keys. A duplicate
	// candidate must reach the strict check rather than evade it via first-match.
	jsonview.Parse(body).ForEach(func(key, value gjson.Result) bool {
		if key.String() != "input" || !value.IsArray() {
			return true
		}
		value.ForEach(func(_, item gjson.Result) bool {
			if item.IsObject() && item.Get("type").String() == "function_call_output" {
				found = true
			}
			return !found
		})
		return !found
	})
	return found
}
