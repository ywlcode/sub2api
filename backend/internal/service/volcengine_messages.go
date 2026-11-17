package service

import (
	"strconv"

	"github.com/tidwall/gjson"
)

// Ark usage values are cumulative snapshots, not deltas. Omitted fields retain
// the previous value, while an explicit valid zero replaces it.
func parseVolcengineMessagesUsage(payload string, usage *ClaudeUsage) {
	root := gjson.Parse(payload)
	var node gjson.Result
	switch root.Get("type").String() {
	case "message_start":
		node = root.Get("message.usage")
	case "message_delta":
		node = root.Get("usage")
	default:
		return
	}
	for _, field := range []struct {
		path string
		dst  *int
	}{
		{"input_tokens", &usage.InputTokens}, {"output_tokens", &usage.OutputTokens},
		{"cache_creation_input_tokens", &usage.CacheCreationInputTokens}, {"cache_read_input_tokens", &usage.CacheReadInputTokens},
		{"cache_creation.ephemeral_5m_input_tokens", &usage.CacheCreation5mTokens}, {"cache_creation.ephemeral_1h_input_tokens", &usage.CacheCreation1hTokens},
	} {
		v := node.Get(field.path)
		if v.Type == gjson.Number {
			if n, err := strconv.Atoi(v.Raw); err == nil && n >= 0 {
				*field.dst = n
			}
		}
	}
}

// These are the semantic completion reasons documented by Ark Messages.
// max_tokens is a complete but truncated response, tool_use hands control back
// to the client; neither warrants a transparent replay of an executed request.
func volcengineAnthropicStopReasonKnown(reason string) bool {
	switch reason {
	case "end_turn", "stop_sequence", "tool_use", "max_tokens":
		return true
	default:
		return false
	}
}

// First token means content, not connection/role/heartbeat/signature metadata.
// A tool block with its name is actionable output even before its JSON deltas.
func volcengineAnthropicHasContent(payload string) bool {
	event := gjson.Parse(payload)
	switch event.Get("type").String() {
	case "content_block_delta":
		delta := event.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			return delta.Get("text").String() != ""
		case "thinking_delta":
			return delta.Get("thinking").String() != ""
		case "input_json_delta":
			return delta.Get("partial_json").String() != ""
		}
	case "content_block_start":
		block := event.Get("content_block")
		switch block.Get("type").String() {
		case "text":
			return block.Get("text").String() != ""
		case "thinking":
			return block.Get("thinking").String() != ""
		case "tool_use", "server_tool_use":
			return block.Get("name").String() != ""
		}
	}
	return false
}
