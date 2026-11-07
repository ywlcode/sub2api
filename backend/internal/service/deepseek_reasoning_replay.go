package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/volcengine"

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

// Keep native DeepSeek and Ark replay on the shared normalization rules.
func normalizeDeepSeekReasoningInput(input any, useSummary bool) bool {
	return volcengine.NormalizeReasoningInput(input, useSummary)
}
