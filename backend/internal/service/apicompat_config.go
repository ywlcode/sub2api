package service

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

func configuredMinOutputTokens(cfg *config.Config) int {
	if cfg != nil && cfg.Gateway.MinOutputTokens >= 16 {
		return cfg.Gateway.MinOutputTokens
	}
	return apicompat.DefaultMinMaxOutputTokens
}

func chatCompletionsToResponses(cfg *config.Config, req *apicompat.ChatCompletionsRequest) (*apicompat.ResponsesRequest, error) {
	return apicompat.ChatCompletionsToResponsesWithMinOutputTokens(req, configuredMinOutputTokens(cfg))
}

func chatCompletionsToResponsesForAccount(cfg *config.Config, account *Account, req *apicompat.ChatCompletionsRequest) (*apicompat.ResponsesRequest, error) {
	return apicompat.ChatCompletionsToResponsesWithOptions(req, apicompat.ResponsesConversionOptions{
		MinOutputTokens:          configuredMinOutputTokens(cfg),
		PreserveReasoningContent: shouldPreserveDeepSeekReasoning(account),
	})
}

func anthropicToResponsesForAccount(cfg *config.Config, account *Account, req *apicompat.AnthropicRequest) (*apicompat.ResponsesRequest, error) {
	return apicompat.AnthropicToResponsesWithOptions(req, apicompat.ResponsesConversionOptions{
		MinOutputTokens:          configuredMinOutputTokens(cfg),
		PreserveReasoningContent: shouldPreserveDeepSeekReasoning(account),
		PreserveReasoningEffort:  account.IsDeepSeekVolcengineCompatibilityEnabled(),
		VolcengineCompatibility:  account.IsDeepSeekVolcengineCompatibilityEnabled(),
	})
}

func responsesToAnthropicForAccount(account *Account, req *apicompat.ResponsesRequest) (*apicompat.AnthropicRequest, error) {
	return apicompat.ResponsesToAnthropicRequestWithOptions(req, apicompat.ResponsesConversionOptions{
		PreserveReasoningContent: shouldPreserveDeepSeekReasoning(account),
	})
}

func settingServiceConfig(settingService *SettingService) *config.Config {
	if settingService == nil {
		return nil
	}
	return settingService.cfg
}
