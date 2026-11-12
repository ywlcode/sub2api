package config

import (
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

func (c *Config) IsSecondary() bool { return c != nil && c.Site.EffectiveMode() == site.Secondary }

// DefaultUsageRateMultiplier retains ordinary configuration on primary/standalone
// sites; replicas use the synchronized value without mutating shared config.
func (c *Config) DefaultUsageRateMultiplier() float64 {
	if c == nil {
		return 1
	}
	if c.IsSecondary() {
		if rate := c.siteRateMultiplier.Load(); rate != nil {
			return *rate
		}
	}
	return c.Default.RateMultiplier
}

func (c *Config) InstallSiteRateMultiplier(rate float64) {
	if c.IsSecondary() {
		c.siteRateMultiplier.Store(&rate)
	}
}

func (c *Config) validateSite() error {
	if err := c.Site.Validate(); err != nil {
		return err
	}
	if c.Site.EffectiveMode() != site.Standalone {
		if c.Runtime.Role == RuntimeRoleGateway || c.Runtime.RegionalAuthFile != "" || c.Runtime.RegionalSchedulerFile != "" || c.Runtime.RegionalFundsFile != "" || c.Runtime.RegionalConcurrencyFile != "" || c.Runtime.ServerlessID != "" {
			return fmt.Errorf("site modes require runtime.role full and cannot use regional/serverless gateway configuration")
		}
		if c.RunMode == RunModeSimple {
			return fmt.Errorf("site modes require normal billing, not simple mode")
		}
	}
	return nil
}
