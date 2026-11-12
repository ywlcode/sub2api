package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// SiteRequestPolicy carries effective request defaults, never a whole config
// file. It contains no database, Redis, proxy or administrator credentials.
type SiteRequestPolicy struct {
	Gateway                 map[string]json.RawMessage `json:"gateway"`
	Instructions            string                     `json:"instructions"`
	MinimumBalanceReserve   float64                    `json:"minimum_balance_reserve"`
	InflightReservation     InflightReservationConfig  `json:"inflight_reservation"`
	OverloadCooldownMinutes int                        `json:"overload_cooldown_minutes"`
	GeminiQuota             GeminiQuotaConfig          `json:"gemini_quota"`
	OpsEnabled              bool                       `json:"ops_enabled"`
}
type siteRequestRuntime struct {
	gateway    GatewayConfig
	billing    BillingConfig
	rateLimit  RateLimitConfig
	gemini     GeminiConfig
	opsEnabled bool
}

func sitePolicyField(v reflect.Value, path string) (reflect.Value, error) {
	for _, part := range strings.Split(path, ".") {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, fmt.Errorf("invalid site policy path %s", path)
		}
		t := v.Type()
		found := false
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).Tag.Get("mapstructure") == part {
				v = v.Field(i)
				found = true
				break
			}
		}
		if !found {
			return reflect.Value{}, fmt.Errorf("unknown site policy path %s", path)
		}
	}
	return v, nil
}

func (c *Config) ExportSiteRequestPolicy() ([]byte, error) {
	p := SiteRequestPolicy{Gateway: make(map[string]json.RawMessage), Instructions: c.Gateway.ForcedCodexInstructionsTemplate,
		MinimumBalanceReserve: c.Billing.MinimumBalanceReserve, InflightReservation: c.Billing.InflightReservation,
		OverloadCooldownMinutes: c.RateLimit.OverloadCooldownMinutes, GeminiQuota: c.Gemini.Quota, OpsEnabled: c.Ops.Enabled}
	for _, path := range siteGatewayPolicyFields {
		field, err := sitePolicyField(reflect.ValueOf(c.Gateway), path)
		if err != nil {
			return nil, err
		}
		p.Gateway[path], err = json.Marshal(field.Interface())
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(p)
}

func (c *Config) InstallSiteRequestPolicy(raw []byte) error {
	if !c.IsSecondary() {
		return fmt.Errorf("request policy installation requires a secondary")
	}
	var p SiteRequestPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if len(p.Gateway) != len(siteGatewayPolicyFields) {
		return fmt.Errorf("incomplete site request policy")
	}
	if p.MinimumBalanceReserve < 0 || p.InflightReservation.TTLSeconds < 0 || p.InflightReservation.DefaultMaxTokens < 0 || p.InflightReservation.MaxInputTokens < 0 || p.InflightReservation.MaxOutputTokens < 0 || p.InflightReservation.MaxReservationUSD < 0 {
		return fmt.Errorf("invalid site billing defaults")
	}
	r := &siteRequestRuntime{gateway: c.Gateway, billing: c.Billing, rateLimit: c.RateLimit, gemini: c.Gemini, opsEnabled: p.OpsEnabled}
	for _, path := range siteGatewayPolicyFields {
		rawValue, ok := p.Gateway[path]
		if !ok {
			return fmt.Errorf("missing site request policy %s", path)
		}
		field, err := sitePolicyField(reflect.ValueOf(&r.gateway).Elem(), path)
		if err != nil {
			return err
		}
		// Decode into fresh storage: never mutate maps/slices shared with the
		// immutable boot config or a prior request snapshot.
		value := reflect.New(field.Type())
		if err = json.Unmarshal(rawValue, value.Interface()); err != nil {
			return err
		}
		field.Set(value.Elem())
	}
	r.gateway.ForcedCodexInstructionsTemplate = p.Instructions
	r.billing.MinimumBalanceReserve, r.billing.InflightReservation = p.MinimumBalanceReserve, p.InflightReservation
	r.rateLimit.OverloadCooldownMinutes = p.OverloadCooldownMinutes
	r.gemini.Quota = p.GeminiQuota
	c.siteRequestPolicy.Store(r)
	return nil
}

func (c *Config) RequestGateway() *GatewayConfig {
	if c.IsSecondary() {
		if p := c.siteRequestPolicy.Load(); p != nil {
			return &p.gateway
		}
	}
	return &c.Gateway
}
func (c *Config) RequestBilling() *BillingConfig {
	if c.IsSecondary() {
		if p := c.siteRequestPolicy.Load(); p != nil {
			return &p.billing
		}
	}
	return &c.Billing
}
func (c *Config) RequestRateLimit() *RateLimitConfig {
	if c.IsSecondary() {
		if p := c.siteRequestPolicy.Load(); p != nil {
			return &p.rateLimit
		}
	}
	return &c.RateLimit
}
func (c *Config) RequestGemini() *GeminiConfig {
	if c.IsSecondary() {
		if p := c.siteRequestPolicy.Load(); p != nil {
			return &p.gemini
		}
	}
	return &c.Gemini
}
func (c *Config) RequestOpsEnabled() bool {
	if c.IsSecondary() {
		if p := c.siteRequestPolicy.Load(); p != nil {
			return p.opsEnabled
		}
	}
	return c.Ops.Enabled
}
