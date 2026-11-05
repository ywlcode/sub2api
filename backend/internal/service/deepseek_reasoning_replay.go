package service

// Both native DeepSeek and Ark DeepSeek replay plaintext reasoning. Other
// providers keep their existing signed/encrypted history conversion.
func shouldPreserveDeepSeekReasoning(account *Account) bool {
	return account.IsDeepSeekVolcengineCompatibilityEnabled() ||
		usesNativeDeepSeekReasoningProtocol(account, APIProtocolResponses) ||
		usesNativeDeepSeekReasoningProtocol(account, APIProtocolChatCompletions) ||
		usesNativeDeepSeekReasoningProtocol(account, APIProtocolAnthropic)
}

// Select the actual protocol base URL. A DeepSeek platform account can point at
// Ark or another relay; its platform label alone does not establish this dialect.
func usesNativeDeepSeekReasoningProtocol(account *Account, protocol string) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	baseURL := account.GetOpenAIBaseURL()
	if account.IsAdaptiveAPIProtocol() {
		baseURL = account.GetCNProtocolBaseURL(protocol)
	} else if protocol == APIProtocolAnthropic && account.IsAnthropicProtocol() {
		baseURL = account.GetAnthropicProtocolBaseURL()
	}
	return isDeepSeekAPIHost(baseURL)
}

// Ark uses summary_text for DeepSeek's full plaintext reasoning, while native
// DeepSeek reads content[].reasoning_text and ignores summary. Translate only
// available plaintext; ciphertext alone cannot reconstruct the original thought.
func normalizeDeepSeekReasoningInput(input any, useSummary bool) bool {
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
