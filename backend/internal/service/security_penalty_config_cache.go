package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"
)

// This reserved SHA-256 identifier fits the existing durable auth outbox.
// Migration 337 enqueues it transactionally for all three settings, including
// direct SQL edits. It carries no credential or configuration payload.
const securityPenaltyConfigInvalidationKey = "06d981b93f14b6407a0488f3be12b23cdddc8a22385b497573639edf9726a4ef"

// Successful snapshots have no timed expiry while invalidations are connected.
// Errors fail closed; offline subscriptions bypass this cache altogether.
// requestConfigCache coalesces refreshes without holding its mutex over I/O,
// lets canceled followers leave, and fences loads against concurrent saves.
func (s *SecurityPenaltyService) loadConfig(ctx context.Context) (SecurityPenaltyConfig, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	values, err := s.settings.GetMultiple(ctx, []string{SettingKeySecurityPenalty, SettingKeyRiskControlEnabled, SettingKeyCyberPolicyUserAllowlist})
	if err != nil {
		return SecurityPenaltyConfig{}, 0, err
	}
	cfg := DefaultSecurityPenaltyConfig()
	if raw := values[SettingKeySecurityPenalty]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return SecurityPenaltyConfig{}, 0, errors.New("invalid security penalty config")
		}
	}
	if err := validateSecurityPenaltyConfig(cfg); err != nil {
		return SecurityPenaltyConfig{}, 0, err
	}
	cfg.RiskControlEnabled = values[SettingKeyRiskControlEnabled] == "true"
	globalExempt, err := ParseCyberPolicyUserAllowlist(values[SettingKeyCyberPolicyUserAllowlist])
	if err != nil {
		return SecurityPenaltyConfig{}, 0, err
	}
	for id := range globalExempt {
		cfg.trustedUserIDs = append(cfg.trustedUserIDs, id)
	}
	cost := 256 + 8*(len(cfg.GroupIDs)+len(cfg.ExemptUserIDs)+len(cfg.trustedUserIDs))
	return cfg, cost, nil
}

func cloneSecurityPenaltyConfig(cfg SecurityPenaltyConfig) SecurityPenaltyConfig {
	cfg.TargetGroupID = cloneGroupValuePointer(cfg.TargetGroupID)
	cfg.GroupIDs = slices.Clone(cfg.GroupIDs)
	cfg.ExemptUserIDs = slices.Clone(cfg.ExemptUserIDs)
	cfg.trustedUserIDs = slices.Clone(cfg.trustedUserIDs)
	return cfg
}

func (s *APIKeyService) invalidateSecurityPenaltyConfig() {
	if penalties := s.securityPenaltyConfig.Load(); penalties != nil {
		penalties.configCache.invalidate(nil)
	}
}

func (s *SecurityPenaltyService) invalidateConfig() {
	s.configCache.invalidate(nil)
	if s.keys == nil || s.keys.cache == nil {
		return
	}
	// Immediate notification is an optimization. The transactional outbox
	// retries a failed publish, and replays once more after its safety delay.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.keys.cache.PublishAuthCacheInvalidation(ctx, securityPenaltyConfigInvalidationKey); err != nil {
		slog.Warn("security penalty configuration notification failed; durable outbox will retry", "error", err)
	}
}
