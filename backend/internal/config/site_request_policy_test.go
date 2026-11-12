package config

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
)

func TestSiteRequestPolicyEffectiveDefaultsAndLocalResources(t *testing.T) {
	p := &Config{}
	p.Gateway.TextMaxBodySize = 123456
	p.Gateway.StreamDataIntervalTimeout = 90
	p.Gateway.MaxUpstreamClients = 99
	p.Gateway.OpenAIWS.MaxConnsPerAccount = 123
	p.Gateway.ForcedCodexInstructionsTemplate = "primary template"
	p.Billing.MinimumBalanceReserve = 2.5
	p.Ops.Enabled = true
	raw, err := p.ExportSiteRequestPolicy()
	require.NoError(t, err)
	s := &Config{Site: site.Config{Mode: site.Secondary}}
	s.Gateway.TextMaxBodySize = 123
	s.Gateway.MaxUpstreamClients = 7
	s.Gateway.OpenAIWS.MaxConnsPerAccount = 3
	require.NoError(t, s.InstallSiteRequestPolicy(raw))
	require.Equal(t, int64(123456), s.RequestGateway().TextMaxBodySize)
	require.Equal(t, 90, s.RequestGateway().StreamDataIntervalTimeout)
	require.Equal(t, 7, s.RequestGateway().MaxUpstreamClients)
	require.Equal(t, 3, s.RequestGateway().OpenAIWS.MaxConnsPerAccount)
	require.Equal(t, "primary template", s.RequestGateway().ForcedCodexInstructionsTemplate)
	require.Equal(t, 2.5, s.RequestBilling().MinimumBalanceReserve)
	require.True(t, s.RequestOpsEnabled())
	require.Equal(t, int64(123), s.Gateway.TextMaxBodySize)
	require.Error(t, p.InstallSiteRequestPolicy(raw))
	require.Equal(t, &p.Gateway, p.RequestGateway())
	var invalid SiteRequestPolicy
	require.NoError(t, json.Unmarshal(raw, &invalid))
	delete(invalid.Gateway, "text_max_body_size")
	bad, _ := json.Marshal(invalid)
	require.Error(t, s.InstallSiteRequestPolicy(bad))
	require.Equal(t, int64(123456), s.RequestGateway().TextMaxBodySize)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				require.Equal(t, int64(123456), s.RequestGateway().TextMaxBodySize)
			}
		})
	}
	for range 30 {
		require.NoError(t, s.InstallSiteRequestPolicy(raw))
	}
	wg.Wait()
}
