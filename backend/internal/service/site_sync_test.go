package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

type siteUsageIssueRecorder struct {
	UsageBillingRepository
	issues []SiteUsageIssue
	err    error
}

func (r *siteUsageIssueRecorder) RecordSiteUsageIssue(_ context.Context, issue SiteUsageIssue) error {
	r.issues = append(r.issues, issue)
	return r.err
}

func TestSiteMissingUsageCreatesReconciliationIssueInsteadOfFreeBill(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		r := &siteUsageIssueRecorder{}
		s := &OpenAIGatewayService{cfg: &config.Config{Site: site.Config{Mode: site.Secondary}}, usageBillingRepo: r}
		err := s.RecordUsage(t.Context(), &OpenAIRecordUsageInput{
			Result: &OpenAIForwardResult{Model: "deepseek-chat", RequestID: "request-missing-usage", UsageUnavailable: unavailable},
			APIKey: &APIKey{ID: 2, UserID: 1}, Account: &Account{ID: 3, Platform: PlatformDeepseek},
		})
		require.ErrorContains(t, err, "requires reconciliation")
		require.Len(t, r.issues, 1)
		require.Equal(t, "upstream_usage_unavailable", r.issues[0].Reason)
		require.Equal(t, int64(1), r.issues[0].UserID)
	}
	r := &siteUsageIssueRecorder{err: errors.New("disk full")}
	err := recordSiteUsageIssue(t.Context(), r, &APIKey{ID: 2, UserID: 1}, &Account{ID: 3}, "request-unpriced", "model-without-price", "pricing_unavailable")
	require.ErrorContains(t, err, "disk full", "persistence failure must not appear as successful billing")
}

func TestSiteMissingPriceDoesNotCreateZeroCostUsage(t *testing.T) {
	usage := &openAIRecordUsageLogRepoStub{inserted: true}
	r := &siteUsageIssueRecorder{}
	s := newOpenAIRecordUsageServiceWithBillingRepoForTest(usage, r, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	defer s.StopOpenAICodexTicketHarvester()
	s.cfg.Site.Mode = site.Secondary
	err := s.RecordUsage(t.Context(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "unpriced", Model: "pricing-missing-test-model", Usage: OpenAIUsage{InputTokens: 1200, OutputTokens: 300}},
		APIKey: &APIKey{ID: 2, UserID: 1}, User: &User{ID: 1}, Account: &Account{ID: 3, Type: AccountTypeAPIKey},
	})
	require.ErrorContains(t, err, "requires reconciliation")
	require.Len(t, r.issues, 1)
	require.Equal(t, "pricing_unavailable", r.issues[0].Reason)
	require.Zero(t, usage.calls)
}

func TestSitePricingIncludesPrimaryDefaultMultiplier(t *testing.T) {
	primaryConfig := &config.Config{Site: site.Config{Mode: site.Primary}}
	primaryConfig.Default.RateMultiplier = 2.5
	primary := &PricingService{cfg: primaryConfig, pricingData: map[string]*LiteLLMModelPricing{"deepseek-chat": {}}}
	raw, err := primary.ExportSitePricing()
	require.NoError(t, err)
	replicaConfig := &config.Config{Site: site.Config{Mode: site.Secondary}}
	replicaConfig.Default.RateMultiplier = 1
	replica := &PricingService{cfg: replicaConfig}
	require.NoError(t, replica.InstallSitePricing(raw))
	require.Equal(t, 2.5, replicaConfig.DefaultUsageRateMultiplier())
	require.Equal(t, 1.0, replicaConfig.Default.RateMultiplier, "immutable boot configuration must not be mutated concurrently")
	require.Error(t, replica.InstallSitePricing([]byte(`{"models":{},"default_rate_multiplier":3}`)))
	require.Equal(t, 2.5, replicaConfig.DefaultUsageRateMultiplier())
	primaryConfig.InstallSiteRateMultiplier(9)
	require.Equal(t, 2.5, primaryConfig.DefaultUsageRateMultiplier(), "standalone and primary retain their configured defaults")
}

func TestSiteSettingRefreshAppliesImmediatelyAndRetainsTrust(t *testing.T) {
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary}}
	cfg.SetForwardedClientIPSettings(true, []string{"X-Office-IP"})
	repo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := &SettingService{cfg: cfg, settingRepo: repo}
	require.NoError(t, svc.RefreshSiteReplica(t.Context()))
	p := DefaultPrioritySchedulingConfig()
	p.Enabled = true
	p.TargetTTFTMs = 3100
	raw, _ := json.Marshal(p)
	repo.values["priority_scheduling_v1"] = string(raw)
	repo.values["openai_response_header_timeout_settings"] = `{"enabled":true,"seconds":75}`
	repo.values["account_scheduling_thresholds"] = `{"openai":42}`
	repo.values["openai_apikey_health_breaker_settings"] = `{"enabled":false}`
	require.NoError(t, svc.RefreshSiteReplica(t.Context()))
	require.True(t, svc.prioritySchedulingRuntimeConfig().Enabled)
	require.Equal(t, 3100, svc.prioritySchedulingRuntimeConfig().TargetTTFTMs)
	seconds, enabled := svc.GetOpenAIResponseHeaderTimeoutCached(t.Context())
	require.Equal(t, 75, seconds)
	require.True(t, enabled)
	require.Equal(t, 42, svc.GetAccountSchedulingThresholds(t.Context())["openai"])
	h, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.False(t, h.Enabled)
	require.Equal(t, []string{"X-Office-IP"}, cfg.ForwardedClientIPSettings().Headers)
	delete(repo.values, "priority_scheduling_v1")
	delete(repo.values, "openai_response_header_timeout_settings")
	require.NoError(t, svc.RefreshSiteReplica(t.Context()))
	require.False(t, svc.prioritySchedulingRuntimeConfig().Enabled)
}
func TestSitePolicyExcludesManagementAndSafety(t *testing.T) {
	for _, k := range []string{"risk_control_enabled", "security_penalty_config", "content_moderation_config", "prompt_audit_config", "group_anti_distillation:1", "smtp_password", "admin_api_key", "payment_config"} {
		require.False(t, IsSiteSetting(k), k)
	}
	for _, k := range []string{"priority_scheduling_v1", "stream_timeout_settings", "web_search_emulation_config", "ops_monitoring_enabled"} {
		require.True(t, IsSiteSetting(k), k)
	}
}
