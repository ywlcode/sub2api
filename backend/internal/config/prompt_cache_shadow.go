package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

type PromptCacheShadowConfig struct {
	Enabled            bool    `mapstructure:"enabled"`
	AccountIDs         []int64 `mapstructure:"account_ids"`
	ComputeConcurrency int     `mapstructure:"compute_concurrency"`
	TTLSeconds         int     `mapstructure:"ttl_seconds"`
	MinPrefixTokens    int     `mapstructure:"min_prefix_tokens"`
	PrefixTokensStep   int     `mapstructure:"prefix_tokens_step"`
	MaxEntries         int     `mapstructure:"max_entries"`
	MaxBodyBytes       int     `mapstructure:"max_body_bytes"`
	LogDir             string  `mapstructure:"log_dir"`
	LogRetentionDays   int     `mapstructure:"log_retention_days"`
	LogMaxTotalMB      int     `mapstructure:"log_max_total_mb"`
	LogQueueSize       int     `mapstructure:"log_queue_size"`
}

func (c PromptCacheShadowConfig) Validate() error {
	for _, field := range []struct {
		name       string
		value, max int
	}{
		{"ttl_seconds", c.TTLSeconds, 86400}, {"min_prefix_tokens", c.MinPrefixTokens, 1048576},
		{"prefix_tokens_step", c.PrefixTokensStep, 65536}, {"max_entries", c.MaxEntries, 1000000},
		{"max_body_bytes", c.MaxBodyBytes, 8 << 20},
		{"compute_concurrency", c.ComputeConcurrency, 64},
		{"log_retention_days", c.LogRetentionDays, 90}, {"log_max_total_mb", c.LogMaxTotalMB, 102400},
		{"log_queue_size", c.LogQueueSize, 16384},
	} {
		if field.value < 0 || field.value > field.max {
			return fmt.Errorf("gateway.prompt_cache_shadow.%s must be between 0 and %d (0 uses default)", field.name, field.max)
		}
	}
	if strings.TrimSpace(c.LogDir) != "" && !filepath.IsAbs(c.LogDir) {
		return fmt.Errorf("gateway.prompt_cache_shadow.log_dir must be an absolute path")
	}
	for _, id := range c.AccountIDs {
		if id <= 0 {
			return fmt.Errorf("gateway.prompt_cache_shadow.account_ids must contain positive ids")
		}
	}
	return nil
}
