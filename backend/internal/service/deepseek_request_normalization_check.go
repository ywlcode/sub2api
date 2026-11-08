package service

import (
	"bytes"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/jsonview"
	"github.com/tidwall/gjson"
)

// These conservative checks borrow the immutable wire body. Common text and
// already-normalized history can bypass a full map decode/encode. The existing
// adapters remain the source of truth whenever a rewrite might be necessary;
// no marker survives an account change, bridge, retry, or body modification.
func volcengineRequestNeedsNormalization(body []byte, protocol string) bool {
	if !gjson.ValidBytes(body) {
		return true // Let the existing decoder return its validation error.
	}
	root := jsonview.Parse(body)
	if !root.IsObject() {
		return true
	}
	controls := make(map[string]any)
	fields := normalizationObjectFields(root)
	for _, key := range []string{"reasoning", "reasoning_effort", "thinking", "output_config"} {
		if value, ok := fields[key]; ok {
			var control any
			if err := decodeOpenAIJSONUseNumber([]byte(value.Raw), &control); err != nil {
				return true
			}
			controls[key] = control
		}
	}
	if len(controls) > 0 {
		before, beforeErr := marshalOpenAIUpstreamJSON(controls)
		normalizeVolcengineReasoning(controls, protocol)
		after, afterErr := marshalOpenAIUpstreamJSON(controls)
		if beforeErr != nil || afterErr != nil || !bytes.Equal(before, after) {
			return true
		}
	}
	return jsonArrayAny(fields["input"], func(item gjson.Result) bool {
		if !item.IsObject() {
			return false
		}
		fields := normalizationObjectFields(item)
		if fields["internal_chat_message_metadata_passthrough"].Exists() || volcengineUntypedMessageView(fields) ||
			fields["type"].Str == "reasoning" && deepSeekReasoningItemNeedsNormalization(item, true) {
			return true
		}
		return jsonArrayAny(fields["content"], func(part gjson.Result) bool {
			fields := normalizationObjectFields(part)
			return fields["type"].Str == "output_text" && fields["logprobs"].Exists()
		})
	}) || jsonArrayAny(fields["messages"], func(item gjson.Result) bool {
		return normalizationObjectFields(item)["internal_chat_message_metadata_passthrough"].Exists()
	})
}

func volcengineUntypedMessageView(item map[string]gjson.Result) bool {
	typ := item["type"]
	if typ.Exists() && typ.Type != gjson.Null && (typ.Type != gjson.String || typ.Str != "") {
		return false
	}
	for _, key := range []string{"call_id", "tool_call_id", "tool_calls", "function_call", "arguments", "output", "encrypted_content"} {
		if item[key].Exists() {
			return false
		}
	}
	switch item["role"].Str {
	case "system", "developer", "user", "assistant":
		content := item["content"]
		return content.Type == gjson.String || content.IsArray()
	default:
		return false
	}
}

func deepSeekReasoningItemNeedsNormalization(item gjson.Result, useSummary bool) bool {
	// Decode only the reasoning item, preserving the existing plaintext and
	// ciphertext rules while leaving surrounding user/tool history borrowed.
	var object map[string]any
	if err := decodeOpenAIJSONUseNumber([]byte(item.Raw), &object); err != nil {
		return true
	}
	return normalizeDeepSeekReasoningInput([]any{object}, useSummary)
}

func deepSeekInputNeedsNormalization(body []byte, applyImages, applyReasoning, useSummary bool, imageField string) bool {
	if !gjson.ValidBytes(body) {
		return true
	}
	return jsonArrayAny(normalizationObjectFields(jsonview.Parse(body))["input"], func(item gjson.Result) bool {
		if !item.IsObject() {
			return false
		}
		fields := normalizationObjectFields(item)
		if applyReasoning && fields["type"].Str == "reasoning" && deepSeekReasoningItemNeedsNormalization(item, useSummary) {
			return true
		}
		switch strings.TrimSpace(fields["type"].Str) {
		case "function_call_output", "custom_tool_call_output", "tool_search_output", "tool_search_call_output", "mcp_tool_call_output":
			if deepSeekToolOutputMayHaveMedia(fields["output"], true) {
				return true
			}
		}
		if !applyImages {
			return false
		}
		checkImage := func(part gjson.Result, fields map[string]gjson.Result) bool {
			switch strings.TrimSpace(fields["type"].Str) {
			case "input_image", "image_url", "image":
				var object map[string]any
				if err := decodeOpenAIJSONUseNumber([]byte(part.Raw), &object); err != nil {
					return true
				}
				return aliasDeepSeekResponsesImagePart(object, imageField)
			default:
				return false
			}
		}
		checkPart := func(part gjson.Result) bool { return checkImage(part, normalizationObjectFields(part)) }
		return checkImage(item, fields) || jsonArrayAny(fields["content"], checkPart) || jsonArrayAny(fields["output"], checkPart)
	})
}

// Match the structural candidates accepted by LiftResponsesToolOutputMedia,
// including JSON encoded inside a tool-output string. Only the full adapter
// decides how to lift/reorder media; ordinary tool text stays borrowed.
func deepSeekToolOutputMayHaveMedia(value gjson.Result, decodeString bool) bool {
	if decodeString && value.Type == gjson.String {
		if strings.HasPrefix(value.Str, "data:image/") {
			return true
		}
		text := strings.TrimSpace(value.Str)
		if len(text) == 0 || text[0] != '{' && text[0] != '[' {
			return false
		}
		return deepSeekToolOutputMayHaveMedia(gjson.Parse(text), false)
	}
	if value.IsArray() {
		return jsonArrayAny(value, func(part gjson.Result) bool { return deepSeekToolOutputMayHaveMedia(part, false) })
	}
	if !value.IsObject() {
		return false
	}
	fields := normalizationObjectFields(value)
	switch fields["type"].Str {
	case "input_image", "image_url":
		return true
	}
	return deepSeekToolOutputMayHaveMedia(fields["content"], false)
}

func jsonArrayAny(array gjson.Result, test func(gjson.Result) bool) bool {
	if !array.IsArray() {
		return false
	}
	matched := false
	array.ForEach(func(_, value gjson.Result) bool {
		matched = test(value)
		return !matched
	})
	return matched
}

// encoding/json resolves duplicate keys using the final occurrence. gjson.Map
// keeps the first, so collect explicitly to preserve the adapters' semantics.
func normalizationObjectFields(object gjson.Result) map[string]gjson.Result {
	if !object.IsObject() {
		return nil
	}
	fields := make(map[string]gjson.Result)
	object.ForEach(func(key, value gjson.Result) bool {
		fields[key.Str] = value
		return true
	})
	return fields
}
