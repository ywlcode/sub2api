package config

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSitePolicyOptionalFieldsAndLegacyShape(t *testing.T) {
	p := &Config{}
	p.Gateway.DisableEncryptedAgentOwnerBinding = true
	p.Gateway.OpenAIScheduler.StrictSessionAffinityGroupIDs = []int64{9}
	raw, err := p.ExportSiteRequestPolicy()
	require.NoError(t, err)
	s := &Config{Site: site.Config{Mode: site.Secondary}}
	s.Gateway.RequestBodyMemoryBudgetBytes = 12345
	require.NoError(t, s.InstallSiteRequestPolicy(raw))
	require.True(t, s.RequestGateway().DisableEncryptedAgentOwnerBinding)
	require.Equal(t, []int64{9}, s.RequestGateway().OpenAIScheduler.StrictSessionAffinityGroupIDs)
	require.Equal(t, int64(12345), s.RequestGateway().RequestBodyMemoryBudgetBytes)
	legacy, err := LegacySiteRequestPolicy(raw)
	require.NoError(t, err)
	var policy SiteRequestPolicy
	require.NoError(t, json.Unmarshal(legacy, &policy))
	require.Len(t, policy.Gateway, len(siteGatewayPolicyFields))
	policy.Gateway["future_optional"] = json.RawMessage(`true`)
	unknown, err := json.Marshal(policy)
	require.NoError(t, err)
	require.NoError(t, s.InstallSiteRequestPolicy(unknown))
	require.False(t, s.RequestGateway().DisableEncryptedAgentOwnerBinding)
	require.Empty(t, s.RequestGateway().OpenAIScheduler.StrictSessionAffinityGroupIDs)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	delete(fields, "minimum_balance_reserve")
	missing, err := json.Marshal(fields)
	require.NoError(t, err)
	require.ErrorContains(t, s.InstallSiteRequestPolicy(missing), "minimum_balance_reserve")
}
