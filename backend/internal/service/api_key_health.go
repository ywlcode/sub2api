package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const APIKeyHealthBreakerCredentialKey = "api_key_health_breaker"
const APIKeyHealthBreakerReason = "api_key_health_breaker"

type APIKeyHealthConfig struct {
	Enabled          bool `json:"enabled"`
	WindowSize       int  `json:"window_size"`
	FailureThreshold int  `json:"failure_threshold"`
}

func DefaultAPIKeyHealthConfig() APIKeyHealthConfig {
	return APIKeyHealthConfig{WindowSize: 100, FailureThreshold: 10}
}

func parseAPIKeyHealthConfig(credentials map[string]any) (APIKeyHealthConfig, error) {
	config := DefaultAPIKeyHealthConfig()
	value, exists := credentials[APIKeyHealthBreakerCredentialKey]
	if !exists || value == nil {
		return config, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return config, fmt.Errorf("invalid API key health breaker configuration: %w", err)
	}
	if len(data) == 0 || data[0] != '{' {
		return config, fmt.Errorf("API key health breaker configuration must be an object")
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("invalid API key health breaker configuration: %w", err)
	}
	if config.WindowSize < 1 || config.WindowSize > 10000 || config.FailureThreshold < 1 || config.FailureThreshold > config.WindowSize {
		return config, fmt.Errorf("API key health breaker requires 1 <= failure_threshold <= window_size <= 10000")
	}
	return config, nil
}

func ValidateAPIKeyHealthConfig(credentials map[string]any) error {
	_, err := parseAPIKeyHealthConfig(credentials)
	if err != nil {
		return infraerrors.BadRequest("INVALID_API_KEY_HEALTH_CONFIG", err.Error())
	}
	return nil
}

func (a *Account) APIKeyHealthConfig() APIKeyHealthConfig {
	if a == nil || a.Type != AccountTypeAPIKey {
		return DefaultAPIKeyHealthConfig()
	}
	config, err := parseAPIKeyHealthConfig(a.Credentials)
	if err != nil {
		return DefaultAPIKeyHealthConfig()
	}
	return config
}

// A new generation fences late results from earlier windows and cooldowns.
type APIKeyHealthState struct {
	Generation       int64
	UntilUnix        int64
	TriggeredAtUnix  int64
	TripCount        int
	Failures         int
	Samples          int
	WindowSize       int
	FailureThreshold int
	StatusCode       int
	Message          string
	Tripped          bool
}

type APIKeyHealthCache interface {
	BeginAPIKeyHealthAttempt(context.Context, int64, APIKeyHealthConfig) (*APIKeyHealthState, error)
	RecordAPIKeyHealthResult(ctx context.Context, accountID, generation int64, success bool, statusCode int, message string) (*APIKeyHealthState, error)
	ResetAPIKeyHealth(ctx context.Context, accountID int64) error
}

func (s *RateLimitService) persistAPIKeyHealthTrip(ctx context.Context, account *Account, health *APIKeyHealthState) {
	if s == nil || s.accountRepo == nil || account == nil || health == nil || health.UntilUnix <= 0 {
		return
	}
	state := &TempUnschedState{
		UntilUnix: health.UntilUnix, TriggeredAtUnix: health.TriggeredAtUnix,
		StatusCode: health.StatusCode, MatchedKeyword: APIKeyHealthBreakerReason,
		RuleIndex: -1, Scope: TempUnschedScopeAccount, ErrorMessage: health.Message,
		TriggerCount: int64(health.Failures), TriggerThreshold: health.FailureThreshold,
		TriggerWindowRequests: health.WindowSize, TriggerSamples: health.Samples, BreakerTripCount: health.TripCount,
	}
	encoded, _ := json.Marshal(state)
	until := time.Unix(health.UntilUnix, 0)
	// Never mutate a shared scheduling snapshot.
	blocked := *account
	blocked.TempUnschedulableUntil = &until
	blocked.TempUnschedulableReason = string(encoded)
	s.notifyAccountSchedulingBlocked(&blocked, until, APIKeyHealthBreakerReason)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(persistCtx, account.ID, until, string(encoded)); err != nil {
		logger.L().Warn("apikey.health_breaker_persist_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return // Redis still blocks sends; the next admission retries persistence.
	}
	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.SetTempUnsched(persistCtx, account.ID, state); err != nil {
			logger.L().Warn("apikey.health_breaker_cache_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
	}
	if health.Tripped {
		logger.L().Warn("apikey.health_breaker_tripped", zap.Int64("account_id", account.ID),
			zap.Int("failures", health.Failures), zap.Int("samples", health.Samples),
			zap.Int("trip_count", health.TripCount), zap.Time("until", until))
	}
}

func (s *RateLimitService) resetAPIKeyHealth(ctx context.Context, accountID int64) error {
	if s == nil || s.apiKeyHealth == nil {
		return nil
	}
	return s.apiKeyHealth.ResetAPIKeyHealth(ctx, accountID)
}

func isAPIKeyHealthCooldown(account *Account) bool {
	if account == nil || account.TempUnschedulableUntil == nil || !time.Now().Before(*account.TempUnschedulableUntil) {
		return false
	}
	var state TempUnschedState
	return json.Unmarshal([]byte(account.TempUnschedulableReason), &state) == nil && state.MatchedKeyword == APIKeyHealthBreakerReason
}

func (s *RateLimitService) hasAPIKeyHealthCooldown(ctx context.Context, account *Account) (bool, error) {
	if isAPIKeyHealthCooldown(account) {
		return true, nil
	}
	if s.apiKeyHealth == nil || !account.APIKeyHealthConfig().Enabled {
		return false, nil
	}
	// Another, longer cooldown can own the DB reason while health is blocked in
	// Redis. Probes must preserve that block too; this does not record a sample.
	state, err := s.apiKeyHealth.BeginAPIKeyHealthAttempt(ctx, account.ID, account.APIKeyHealthConfig())
	if err != nil {
		return false, err
	}
	return state != nil && state.UntilUnix > 0, nil
}
