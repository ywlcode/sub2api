package promptcache

import (
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Each canonical record is tokenized independently. Appending a new turn cannot
// move an earlier record's BPE boundary or invalidate its measured usage anchor.
func (e *Engine) promptParts(body []byte, hint string) (parts []string, protocol, layout, identity string, opaque int, opaqueHashes [][32]byte, reason string) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		reason = "invalid_json"
		return
	}
	for _, field := range []string{"previous_response_id", "conversation", "conversation_id", "thread_id", "cached_content", "cachedContent"} {
		if nonempty(root[field]) {
			reason = "server_side_context"
			return
		}
	}
	if nonempty(root["prompt"]) {
		reason = "stored_prompt"
		return
	}
	controls := map[string]json.RawMessage{}
	for _, field := range []string{"tool_choice", "parallel_tool_calls", "response_format", "text", "reasoning", "thinking"} {
		if nonempty(root[field]) {
			controls[field] = root[field]
		}
	}
	appendRaw := func(raw []byte) bool {
		var value any
		if decodeExactJSON(raw, &value) != nil {
			reason = "invalid_prompt"
			return false
		}
		data, err := canonicalJSON(value)
		if err != nil {
			reason = "invalid_prompt"
			return false
		}
		parts = append(parts, string(data)+"\n")
		return true
	}
	controlRaw, _ := json.Marshal(controls)
	if !appendRaw(controlRaw) {
		return
	}
	appendField := func(field string, sanitize, split bool) bool {
		if !nonempty(root[field]) {
			return true
		}
		parts = append(parts, `"`+field+`"`+"\n")
		var value any
		if decodeExactJSON(root[field], &value) != nil {
			reason = "invalid_prompt"
			return false
		}
		if sanitize {
			var err error
			value, err = e.normalizeContent(value, &opaque, 0, &opaqueHashes)
			if err != nil {
				reason = err.Error()
				return false
			}
		}
		items, array := value.([]any)
		if split && array && len(items) > maxAttemptPoints {
			reason = "fingerprint_limit"
			return false
		}
		if !split || !array {
			items = []any{value}
		}
		for _, item := range items {
			data, err := canonicalJSON(item)
			if err != nil {
				reason = "invalid_prompt"
				return false
			}
			parts = append(parts, string(data)+"\n")
		}
		return true
	}
	// Tools contain user-defined JSON Schema property names. They must not be
	// mistaken for encrypted content or media nodes by a recursive key search.
	if !appendField("tools", false, false) {
		return
	}
	if _, exists := root["messages"]; exists && hint != ProtocolResponses {
		protocol, layout = ProtocolChatCompletions, "records_v3_messages"
		if hint == ProtocolMessages || hint == "" && nonempty(root["system"]) {
			protocol = ProtocolMessages
			if !appendField("system", true, false) {
				return
			}
		}
		if !appendField("messages", true, true) {
			return
		}
	} else if _, exists := root["input"]; exists {
		protocol, layout = ProtocolResponses, "records_v3_responses"
		if !appendField("instructions", true, false) || !appendField("input", true, true) {
			return
		}
	} else {
		reason = "unsupported_protocol"
		return
	}
	if hint != "" && protocol != hint {
		reason = "protocol_mismatch"
		return
	}
	var cacheIdentity any
	_ = decodeExactJSON(root["prompt_cache_key"], &cacheIdentity)
	data, _ := json.Marshal(cacheIdentity)
	identity = string(data)
	return
}

func (e *Engine) normalizeContent(value any, opaque *int, depth int, hashes *[][32]byte) (any, error) {
	if depth > 64 {
		return nil, errors.New("prompt_depth_limit")
	}
	switch node := value.(type) {
	case []any:
		for i, child := range node {
			v, err := e.normalizeContent(child, opaque, depth+1, hashes)
			if err != nil {
				return nil, err
			}
			node[i] = v
		}
	case map[string]any:
		kind, _ := node["type"].(string)
		switch kind {
		case "input_image", "output_image", "image_url", "image", "input_audio", "output_audio", "audio", "input_file", "file", "video", "video_url", "input_video":
			return nil, errors.New("media_input")
		case "item_reference":
			return nil, errors.New("server_side_reference")
		}
		role, _ := node["role"].(string)
		if role == "assistant" || role == "user" || role == "system" || role == "developer" || kind == "reasoning" || kind == "compaction" || kind == "function_call" || kind == "function_call_output" {
			delete(node, "id")
			delete(node, "status")
			if kind == "message" {
				delete(node, "type")
			}
		}
		if kind == "input_text" || kind == "output_text" {
			node["type"] = "text"
			delete(node, "annotations")
			delete(node, "logprobs")
		}
		for key, child := range node {
			if key == "encrypted_content" || key == "signature" && (kind == "thinking" || kind == "redacted_thinking") || key == "data" && kind == "redacted_thinking" {
				if child == nil || child == "" {
					continue
				}
				text, ok := child.(string)
				if !ok {
					return nil, errors.New("invalid_opaque_block")
				}
				h := e.digest([]byte(text))
				if len(*hashes) >= maxAttemptPoints {
					return nil, errors.New("fingerprint_limit")
				}
				*hashes = append(*hashes, h)
				node[key] = map[string]string{"shadow_opaque_hmac": hex.EncodeToString(h[:])}
				*opaque++
				continue
			}
			v, err := e.normalizeContent(child, opaque, depth+1, hashes)
			if err != nil {
				return nil, err
			}
			node[key] = v
		}
	}
	return value, nil
}

func (e *Engine) recordDigest(prior [32]byte, record []byte) [32]byte {
	part := e.digest(record)
	var value [65]byte
	value[0] = 3 // Domain separation from token-prefix and namespace fingerprints.
	copy(value[1:33], prior[:])
	copy(value[33:], part[:])
	return e.digest(value[:])
}

// ObserveOutput retains only a chained fingerprint. It does not retain response
// content, invent missing SSE output, or commit anything before success.
func (a *Attempt) ObserveOutput(raw []byte) bool {
	if a == nil || a.SkipReason != "" || len(raw) > a.engine.MaxBodyBytes() {
		return false
	}
	var items []any
	if decodeExactJSON(raw, &items) != nil || len(items) == 0 {
		return false
	}
	var hashes [][32]byte
	value, err := a.engine.normalizeContent(items, new(int), 0, &hashes)
	if err != nil {
		return false
	}
	normalized, ok := value.([]any)
	if !ok {
		return false
	}
	hash := a.recordHash
	for _, item := range normalized {
		data, err := canonicalJSON(item)
		if err != nil {
			return false
		}
		hash = a.engine.recordDigest(hash, append(data, '\n'))
	}
	a.engine.mu.Lock()
	defer a.engine.mu.Unlock()
	// Output identities share the existing bounded in-flight reservation. Only
	// the first complete output is installed; duplicate terminal events are inert.
	if a.OutputIndexed {
		return true
	}
	if a.reserved+len(hashes) > maxAttemptPoints || a.engine.retainedPoints+len(hashes) > maxRetainedPoints {
		return false
	}
	for _, digest := range hashes {
		a.outputOpaqueKeys = append(a.outputOpaqueKeys, a.engine.opaqueKey(a.sharedNamespace, digest))
	}
	a.reserved += len(hashes)
	a.engine.retainedPoints += len(hashes)
	a.outputHash, a.OutputIndexed = hash, true
	return true
}
