// Package volcengine contains the stateless Ark wire compatibility shared by
// the full gateway and the database-free relay.
package volcengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	ProtocolResponses       = "responses"
	ProtocolChatCompletions = "chat_completions"
	ProtocolAnthropic       = "anthropic"
)

// NormalizeRequest only translates known compatibility aliases.
// Unknown fields, tool arguments, literal tags and user text are not scrubbed.
func NormalizeRequest(body []byte, protocol string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var request map[string]any
	if err := decodeJSON(body, &request); err != nil {
		return nil, fmt.Errorf("normalize Volcengine request: %w", err)
	}
	if request == nil {
		return body, nil
	}
	normalizeVolcengineReasoning(request, protocol)
	NormalizeReasoningInput(request["input"], true)
	for _, field := range []string{"input", "messages"} {
		items, _ := request[field].([]any)
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			delete(item, "internal_chat_message_metadata_passthrough")
			if field == "input" {
				if volcengineUntypedMessage(item) {
					item["type"] = "message"
				}
				parts, _ := item["content"].([]any)
				for _, value := range parts {
					if part, ok := value.(map[string]any); ok && part["type"] == "output_text" {
						delete(part, "logprobs")
					}
				}
			}
		}
	}
	return marshalJSON(request)
}

func volcengineUntypedMessage(item map[string]any) bool {
	if typ, exists := item["type"]; exists && typ != nil && typ != "" {
		return false
	}
	// Do not relabel tool/reasoning items that happen to carry role/content.
	for _, key := range []string{"call_id", "tool_call_id", "tool_calls", "function_call", "arguments", "output", "encrypted_content"} {
		if _, exists := item[key]; exists {
			return false
		}
	}
	switch item["role"] {
	case "system", "developer", "user", "assistant":
	default:
		return false
	}
	switch item["content"].(type) {
	case string, []any:
		return true
	}
	return false
}

func volcengineEffort(value any) string {
	effort, _ := value.(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "off", "disabled":
		return "none"
	default:
		return effort
	}
}

func normalizeVolcengineReasoning(request map[string]any, protocol string) {
	thinking, _ := request["thinking"].(map[string]any)
	mode := volcengineEffort(thinking["type"])
	if protocol == ProtocolAnthropic {
		output, _ := request["output_config"].(map[string]any)
		effort := volcengineEffort(output["effort"])
		if mode == "none" || effort == "none" {
			request["thinking"] = map[string]any{"type": "disabled"}
			delete(output, "effort")
			if len(output) == 0 {
				delete(request, "output_config")
			}
		} else if effort != "" {
			output["effort"] = effort
		}
		return
	}
	// Native nested effort wins over the legacy flat alias, as in the bridges.
	reasoning, _ := request["reasoning"].(map[string]any)
	effort := volcengineEffort(reasoning["effort"])
	if effort == "" {
		effort = volcengineEffort(request["reasoning_effort"])
	}
	if mode == "none" {
		effort = "none"
	}
	if effort == "" {
		return
	}
	// Use a single control so Ark never sees disabled + high, or enabled + none.
	if mode == "none" || mode == "enabled" {
		delete(request, "thinking")
	}
	if protocol == ProtocolChatCompletions {
		request["reasoning_effort"] = effort
		delete(request, "reasoning")
	} else {
		if reasoning == nil {
			reasoning = make(map[string]any)
		}
		reasoning["effort"] = effort
		request["reasoning"] = reasoning
		delete(request, "reasoning_effort")
	}
}

// Ark uses summary_text for DeepSeek's full plaintext reasoning, while native
// DeepSeek reads content[].reasoning_text and ignores summary. Translate only
// available plaintext; ciphertext alone cannot reconstruct the original thought.
func NormalizeReasoningInput(input any, useSummary bool) bool {
	items, _ := input.([]any)
	changed := false
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok || item["type"] != "reasoning" {
			continue
		}
		content := deepSeekReasoningParts(item["content"], "reasoning_text")
		if useSummary {
			if len(content) == 0 {
				continue
			}
			item["summary"] = deepSeekReasoningWireParts(content, "summary_text")
			delete(item, "content")
			changed = true
			continue
		}
		if len(content) > 0 {
			// Raw reasoning is more complete than a potentially abbreviated
			// summary. Never replace it with the summary when both exist.
			continue
		}
		if encrypted, _ := item["encrypted_content"].(string); encrypted != "" {
			// A summary accompanying ciphertext may not contain the original
			// reasoning. Do not present it as recovered plaintext.
			continue
		}
		summary := deepSeekReasoningParts(item["summary"], "summary_text")
		if len(summary) == 0 {
			continue
		}
		item["content"] = deepSeekReasoningWireParts(summary, "reasoning_text")
		delete(item, "summary")
		changed = true
	}
	return changed
}

func deepSeekReasoningParts(value any, partType string) []string {
	if text, ok := value.(string); ok {
		if text != "" {
			return []string{text}
		}
		return nil
	}
	var texts []string
	parts, _ := value.([]any)
	for _, value := range parts {
		part, ok := value.(map[string]any)
		if !ok || part["type"] != partType {
			continue
		}
		if text, ok := part["text"].(string); ok && text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func deepSeekReasoningWireParts(texts []string, partType string) []any {
	parts := make([]any, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, map[string]any{"type": partType, "text": text})
	}
	return parts
}

func decodeJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
