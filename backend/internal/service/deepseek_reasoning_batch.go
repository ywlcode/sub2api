package service

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/jsonview"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// batchDeepSeekReasoningPlaceholders changes individual messages, then assembles
// the body once. Unchanged spans (including real reasoning and tool calls) stay
// byte-for-byte intact. Ambiguous/nonstandard input uses the original algorithm.
func batchDeepSeekReasoningPlaceholders(body []byte) ([]byte, bool) {
	// Keep Get/Array semantics identical to the old implementation, even for
	// malformed input: gjson.ForEach and Array do not always walk it alike.
	messages := jsonview.Get(body, "messages")
	if !messages.IsArray() {
		return body, true
	}
	type replacement struct {
		index int
		msg   gjson.Result
		body  []byte
	}
	var replacements []replacement
	for i, msg := range messages.Array() {
		if strings.TrimSpace(msg.Get("role").String()) == "assistant" && msg.Get("reasoning_content").String() == "" {
			replacements = append(replacements, replacement{index: i, msg: msg})
		}
	}
	if len(replacements) == 0 {
		return body, true
	}
	// One missing field uses exactly the original single sjson edit. No separate
	// validation or temporary message buffer is needed on this common short path.
	if len(replacements) == 1 {
		updated, err := sjson.SetBytes(body, "messages."+strconv.Itoa(replacements[0].index)+".reasoning_content", deepSeekChatReasoningPlaceholderText)
		return updated, err == nil
	}
	if !gjson.ValidBytes(body) {
		return body, false
	}
	count := 0
	jsonview.Parse(body).ForEach(func(key, _ gjson.Result) bool {
		if key.String() == "messages" {
			count++
		}
		return true
	})
	if count != 1 {
		return body, false
	}
	size, previousEnd := len(body), 0
	for i := range replacements {
		patch := &replacements[i]
		msg := patch.msg
		if !msg.IsObject() {
			return body, false
		}
		seen, unique := make(map[string]bool), true
		msg.ForEach(func(key, _ gjson.Result) bool {
			name := key.String()
			if seen[name] {
				unique = false
			}
			seen[name] = true
			return unique
		})
		start, end := msg.Index, msg.Index+len(msg.Raw)
		if !unique || start < previousEnd || end > len(body) || !bytes.Equal(body[start:end], []byte(msg.Raw)) {
			return body, false
		}
		updated, err := sjson.SetBytes(body[start:end], "reasoning_content", deepSeekChatReasoningPlaceholderText)
		if err != nil {
			return body, false
		}
		patch.body = updated
		size += len(updated) - (end - start)
		previousEnd = end
	}
	out := make([]byte, 0, size)
	pos := 0
	for _, patch := range replacements {
		out = append(out, body[pos:patch.msg.Index]...)
		out = append(out, patch.body...)
		pos = patch.msg.Index + len(patch.msg.Raw)
	}
	out = append(out, body[pos:]...)
	return out, true
}
