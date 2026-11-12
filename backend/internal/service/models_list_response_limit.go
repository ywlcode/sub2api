package service

import "github.com/Wei-Shaw/sub2api/internal/config"

func resolveModelsListReadLimit(cfg *config.Config) int64 {
	if cfg != nil && cfg.RequestGateway().ModelsListReadMaxBytes > 0 {
		return cfg.RequestGateway().ModelsListReadMaxBytes
	}
	return config.DefaultModelsListReadMaxBytes
}
