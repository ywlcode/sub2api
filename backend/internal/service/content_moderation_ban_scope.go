package service

import "strings"

const (
	ContentModerationBanScopeAll    = "all"
	ContentModerationBanScopeOpenAI = "openai"
)

// Empty values preserve the behavior of configurations saved before scopes existed.
func NormalizeContentModerationBanScope(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return ContentModerationBanScopeAll
	}
	return scope
}

func ValidContentModerationBanScope(scope string) bool {
	switch NormalizeContentModerationBanScope(scope) {
	case ContentModerationBanScopeAll, ContentModerationBanScopeOpenAI:
		return true
	default:
		return false
	}
}
