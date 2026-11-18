package service

import (
	"encoding/json"
	"fmt"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const UpstreamTransportCooldownCredentialKey = "upstream_transport_cooldown"

// UpstreamTransportCooldownConfig controls only the immediate pause after a
// persistent transport failure. Health breakers and HTTP error rules are
// independent policies and must not consult this configuration.
type UpstreamTransportCooldownConfig struct {
	Enabled         bool `json:"enabled"`
	DurationMinutes int  `json:"duration_minutes"`
}

func DefaultUpstreamTransportCooldownConfig() UpstreamTransportCooldownConfig {
	return UpstreamTransportCooldownConfig{Enabled: true, DurationMinutes: 10}
}

func parseUpstreamTransportCooldownConfig(credentials map[string]any) (UpstreamTransportCooldownConfig, error) {
	config := DefaultUpstreamTransportCooldownConfig()
	value := credentials[UpstreamTransportCooldownCredentialKey]
	if value == nil {
		return config, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return config, fmt.Errorf("invalid upstream transport cooldown: %w", err)
	}
	if len(data) == 0 || data[0] != '{' {
		return config, fmt.Errorf("upstream transport cooldown must be an object")
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("invalid upstream transport cooldown: %w", err)
	}
	if config.DurationMinutes < 1 || config.DurationMinutes > 1440 {
		return config, fmt.Errorf("upstream transport cooldown duration_minutes must be an integer between 1 and 1440")
	}
	return config, nil
}

func ValidateUpstreamTransportCooldownConfig(credentials map[string]any) error {
	if _, err := parseUpstreamTransportCooldownConfig(credentials); err != nil {
		return infraerrors.BadRequest("INVALID_UPSTREAM_TRANSPORT_COOLDOWN", err.Error())
	}
	return nil
}

func (a *Account) UpstreamTransportCooldown() time.Duration {
	config := DefaultUpstreamTransportCooldownConfig()
	if a != nil {
		if stored, err := parseUpstreamTransportCooldownConfig(a.Credentials); err == nil {
			config = stored
		}
	}
	if !config.Enabled {
		return 0
	}
	return time.Duration(config.DurationMinutes) * time.Minute
}
