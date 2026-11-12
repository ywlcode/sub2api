package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// A missing usage/price is a durable reconciliation issue, never an inferred
// free request. Secondary repositories record it in Ops and the upload outbox.
type SiteUsageIssue struct {
	RequestID                   string
	UserID, APIKeyID, AccountID int64
	Model, Reason               string
}

func recordSiteUsageIssue(ctx context.Context, repo UsageBillingRepository, key *APIKey, account *Account, requestID, model, reason string) error {
	writer, ok := repo.(interface {
		RecordSiteUsageIssue(context.Context, SiteUsageIssue) error
	})
	if !ok || key == nil || account == nil {
		return errors.New("site usage reconciliation storage unavailable")
	}
	issue := SiteUsageIssue{RequestID: resolveUsageBillingRequestID(ctx, requestID), UserID: key.UserID, APIKeyID: key.ID, AccountID: account.ID, Model: model, Reason: reason}
	if err := writer.RecordSiteUsageIssue(ctx, issue); err != nil {
		return err
	}
	return fmt.Errorf("site usage requires reconciliation: %s", reason)
}

type sitePricingProjection struct {
	Models                map[string]*LiteLLMModelPricing `json:"models"`
	DefaultRateMultiplier float64                         `json:"default_rate_multiplier"`
}

func (s *PricingService) ExportSitePricing() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.Marshal(sitePricingProjection{Models: s.pricingData, DefaultRateMultiplier: s.cfg.DefaultUsageRateMultiplier()})
}

func (s *PricingService) InstallSitePricing(raw []byte) error {
	var data sitePricingProjection
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if len(data.Models) == 0 {
		return errors.New("primary pricing catalog is empty")
	}
	if math.IsNaN(data.DefaultRateMultiplier) || math.IsInf(data.DefaultRateMultiplier, 0) || data.DefaultRateMultiplier < 0 {
		return errors.New("invalid primary default rate multiplier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pricingData = data.Models
	s.cfg.InstallSiteRateMultiplier(data.DefaultRateMultiplier)
	s.lastUpdated = time.Now()
	return nil
}

func (s *ChannelService) UseSiteReplica() { s.siteReplica.Store(true) }

func (s *ChannelService) RefreshSiteReplica(ctx context.Context) error {
	_, err := s.buildCache(ctx)
	return err
}

func (s *SettingService) RefreshSiteReplica(ctx context.Context) error {
	values, err := s.settingRepo.GetAll(ctx)
	if err != nil {
		return err
	}
	settings := s.parseSettings(values)
	// IP trust follows the receiving node's reverse-proxy topology.
	if s.cfg != nil {
		local := s.cfg.ForwardedClientIPSettings()
		settings.APIKeyACLTrustForwardedIP, settings.ForwardedClientIPHeaders = local.TrustForwardedIP, local.Headers
	}
	priority, err := s.GetPrioritySchedulingConfig(ctx)
	if err != nil {
		return err
	}
	headerTimeout, err := s.GetOpenAIResponseHeaderTimeoutSettings(ctx)
	if err != nil {
		return err
	}
	search := parseWebSearchConfigJSON(values[SettingKeyWebSearchEmulationConfig])
	if err = validateWebSearchConfig(search); err != nil {
		return err
	}
	health := *DefaultOpenAIAPIKeyHealthBreakerSettings()
	if raw := values[SettingKeyOpenAIAPIKeyHealthBreakerSettings]; raw != "" {
		if err = json.Unmarshal([]byte(raw), &health); err != nil {
			return err
		}
	}
	// Wait for old in-flight cache loads before invalidating their results.
	s.siteReplicaMu.Lock()
	defer s.siteReplicaMu.Unlock()
	versionBoundsCache.Store(&cachedVersionBounds{})
	versionBoundsSF.Forget("version_bounds")
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
	gatewayForwardingSF.Forget("gateway_forwarding")
	s.codexRestrictionPolicyCache.Store(&cachedCodexRestrictionPolicy{})
	s.codexRestrictionPolicySF.Forget("codex_restriction_policy")
	s.modelBillingCache.mu.Lock()
	s.modelBillingCache.expires = time.Time{}
	s.modelBillingCache.mu.Unlock()
	s.prioritySchedulingConfig.mu.Lock()
	s.prioritySchedulingConfig.revision++
	s.prioritySchedulingConfig.config = priority
	s.prioritySchedulingConfig.expires = time.Now().Add(5 * time.Second)
	s.prioritySchedulingConfig.mu.Unlock()
	s.refreshCachedSettings(settings)
	s.safetyPromptRuntimeCache.Store(&cachedSafetyPromptRuntime{})
	s.openAIAPIKeyHealthBreakerCache.Store(&cachedOpenAIAPIKeyHealthBreakerSettings{settings: *normalizeOpenAIAPIKeyHealthBreakerSettings(&health), expiresAt: time.Now().Add(openAIAPIKeyHealthBreakerSettingsCacheTTL)})
	s.openAIResponseHeaderTimeoutMu.Lock()
	s.openAIResponseHeaderTimeoutGen++
	s.openAIResponseHeaderTimeoutSF.Forget("openai_response_header_timeout_settings")
	s.storeOpenAIResponseHeaderTimeoutCache(*headerTimeout, openAIResponseHeaderTimeoutCacheTTL)
	s.openAIResponseHeaderTimeoutMu.Unlock()
	webSearchEmulationSF.Forget(sfKeyWebSearchConfig)
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{config: search, expiresAt: time.Now().Add(webSearchEmulationCacheTTL).UnixNano()})
	if s.webSearchManagerBuilder != nil {
		s.webSearchManagerBuilder(search, s.resolveProviderProxyURLs(ctx, search))
	}
	return nil
}

func (s *TLSFingerprintProfileService) RefreshSiteReplica(ctx context.Context) error {
	return s.reloadFromDB(ctx)
}

func (s *ErrorPassthroughService) RefreshSiteReplica(ctx context.Context) error {
	return s.reloadRulesFromDB(ctx)
}

// InvalidateSiteReplica is synchronous: no new replica lease is published until
// policy caches have been invalidated. Auth/billing/scheduler reads on secondary
// nodes use their local database directly, independently of Redis availability.
func (s *APIKeyService) InvalidateSiteReplica() {
	s.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "all:0")
	if p := s.groupAntiDistillation; p != nil {
		p.mu.Lock()
		p.configEpoch++
		p.configs = make(map[int64]groupAntiDistillationCachedConfig)
		p.mu.Unlock()
	}
}

// Imported event receipts and canonical money are committed together. Retrying
// these cache invalidations is safe, unlike replaying a Redis balance decrement.
func (s *BillingCacheService) InvalidateSiteUsage(ctx context.Context, events []UsageBillingEvent, auth APIKeyAuthCacheInvalidator) error {
	for _, e := range events {
		if err := invalidateDurableUsageBilling(ctx, &UsageBillingRecovery{Event: e, NeedsCache: true, Result: UsageBillingApplyResult{APIKeyQuotaExhausted: e.Command.APIKeyQuotaCost > 0}}, s, auth); err != nil {
			return err
		}
	}
	return nil
}
