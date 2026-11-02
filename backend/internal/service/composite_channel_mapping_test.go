//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCompositeChannelMappingSelectsTargetAccount(t *testing.T) {
	const publicModel, targetModel = "gpt-5.6-luna", "gpt-6.1-sol"
	groupID := int64(77)
	cooldown := time.Now().Add(time.Hour)
	owner := Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
		TempUnschedulableUntil: &cooldown,
		Credentials:            map[string]any{"model_mapping": map[string]any{publicModel: publicModel, targetModel: targetModel}},
	}
	target := Account{
		ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
		Credentials: map[string]any{"model_mapping": map[string]any{targetModel: targetModel}},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	repo := schedulerTestOpenAIAccountRepo{accounts: []Account{owner, target}}
	channels := newTestChannelService(makeStandardRepo(Channel{
		ID: 1, Status: StatusActive, GroupIDs: []int64{groupID},
		ModelMapping:       map[string]map[string]string{PlatformOpenAI: {publicModel: targetModel}},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceRequested,
		ModelPricing:       []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{publicModel}}},
	}, map[int64]string{groupID: PlatformComposite}))
	ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched: true, Source: CompositeRouteSourceAccount, GroupID: groupID,
		PublicModel: publicModel, TargetPlatform: PlatformOpenAI, UpstreamModel: publicModel,
	})
	mapping, _ := channels.ResolveChannelMappingAndRestrict(ctx, &groupID, publicModel)
	require.True(t, mapping.Mapped)
	ctx = mapping.WithModelRoutingContext(ctx, publicModel)

	for _, mode := range []string{"true", "false"} {
		for _, path := range []string{"initial", "sticky", "websocket", "database_recheck"} {
			t.Run(mode+"/"+path, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				cfg := &config.Config{RunMode: config.RunModeStandard}
				cfg.Gateway.OpenAIWS.Enabled = true
				cfg.Gateway.OpenAIWS.APIKeyEnabled = true
				cfg.Gateway.OpenAIWS.ResponsesWebsockets = true
				cache := &schedulerTestGatewayCache{}
				svc := &OpenAIGatewayService{
					accountRepo: repo, cfg: cfg, cache: cache,
					channelService:     channels,
					rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(mode),
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
				}
				sessionHash := ""
				transport := OpenAIUpstreamTransportAny
				switch path {
				case "sticky":
					sessionHash = "target-session"
					cache.sessionBindings = map[string]int64{"openai:" + sessionHash: target.ID}
				case "websocket":
					transport = OpenAIUpstreamTransportResponsesWebsocket
				case "database_recheck":
					staleTarget := target
					staleTarget.Credentials = map[string]any{"model_mapping": map[string]any{publicModel: publicModel, targetModel: targetModel}}
					svc.schedulerSnapshot = &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{
						snapshotAccounts: []*Account{&owner, &staleTarget},
						accountsByID:     map[int64]*Account{owner.ID: &owner, target.ID: &staleTarget},
					}}
				}
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", sessionHash, mapping.MappedModel, nil, transport, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, target.ID, selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestCompositeChannelMappingGenericGatewaySelectsTargetAccount(t *testing.T) {
	const publicModel, targetModel = "team-alias", "claude-sonnet-4-6"
	groupID := int64(77)
	account := Account{
		ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		AccountGroups: []AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"model_mapping": map[string]any{targetModel: targetModel}},
	}
	group := &Group{ID: groupID, Platform: PlatformComposite, Status: StatusActive, Hydrated: true}
	for _, loadAware := range []bool{false, true} {
		for _, billingSource := range []string{BillingModelSourceRequested, BillingModelSourceChannelMapped} {
			pricingModel := publicModel
			if billingSource == BillingModelSourceChannelMapped {
				pricingModel = targetModel
			}
			channels := newTestChannelService(makeStandardRepo(Channel{
				ID: 1, Status: StatusActive, GroupIDs: []int64{groupID},
				ModelMapping:       map[string]map[string]string{PlatformAnthropic: {publicModel: targetModel}},
				RestrictModels:     true,
				BillingModelSource: billingSource,
				ModelPricing:       []ChannelModelPricing{{Platform: PlatformAnthropic, Models: []string{pricingModel}}},
			}, map[int64]string{groupID: PlatformComposite}))
			cfg := testConfig()
			cfg.Gateway.Scheduling.LoadBatchEnabled = loadAware
			svc := &GatewayService{
				accountRepo: &mockAccountRepoForPlatform{
					accounts: []Account{account}, accountsByID: map[int64]*Account{account.ID: &account},
				},
				groupRepo:          &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}},
				channelService:     channels,
				cfg:                cfg,
				concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
			}
			ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
				Matched: true, Source: CompositeRouteSourceAccount, GroupID: groupID,
				PublicModel: publicModel, TargetPlatform: PlatformAnthropic, UpstreamModel: publicModel,
			})
			mapping, _ := channels.ResolveChannelMappingAndRestrict(ctx, &groupID, publicModel)
			ctx = mapping.WithModelRoutingContext(ctx, publicModel)
			selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", targetModel, nil, "", 0)
			require.NoError(t, err, "load aware: %v, billing: %s", loadAware, billingSource)
			require.Equal(t, account.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		}
	}
}

func TestCompositeChannelMappingPreservesAliasProtection(t *testing.T) {
	const publicModel, targetModel = "team-alias", "gpt-6.1-sol"
	groupID := int64(77)
	ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched: true, Source: CompositeRouteSourceAccount, GroupID: groupID,
		PublicModel: publicModel, TargetPlatform: PlatformOpenAI, UpstreamModel: publicModel,
	})
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{targetModel: targetModel}},
	}
	for _, tc := range []struct {
		name, source, routingModel string
		mapping                    ChannelMappingResult
		want                       bool
	}{
		{name: "channel rewrite", source: publicModel, routingModel: targetModel, mapping: ChannelMappingResult{Mapped: true, MappedModel: targetModel}, want: true},
		{name: "no mapping", source: publicModel, routingModel: targetModel},
		{name: "identity mapping", source: publicModel, routingModel: targetModel, mapping: ChannelMappingResult{Mapped: true, MappedModel: publicModel}},
		{name: "empty target", source: publicModel, routingModel: targetModel, mapping: ChannelMappingResult{Mapped: true}},
		{name: "unrelated source", source: "another-alias", routingModel: targetModel, mapping: ChannelMappingResult{Mapped: true, MappedModel: targetModel}},
		{name: "different routing target", source: publicModel, routingModel: "gpt-6-sol", mapping: ChannelMappingResult{Mapped: true, MappedModel: targetModel}},
		{name: "target unsupported", source: publicModel, routingModel: "gpt-6-sol", mapping: ChannelMappingResult{Mapped: true, MappedModel: "gpt-6-sol"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestCtx := tc.mapping.WithModelRoutingContext(ctx, tc.source)
			require.Equal(t, tc.want, isOpenAICompatibleAccountEligibleForRequest(requestCtx, account, &groupID, PlatformOpenAI, tc.routingModel, false, ""))
			require.Equal(t, tc.want, (&GatewayService{}).isModelSupportedByAccountInGroup(requestCtx, account, &groupID, tc.routingModel))
		})
	}

	// Reusing a context for another turn must not retain a previous exemption.
	rewritten := (ChannelMappingResult{Mapped: true, MappedModel: targetModel}).WithModelRoutingContext(ctx, publicModel)
	unmapped := (ChannelMappingResult{}).WithModelRoutingContext(rewritten, publicModel)
	require.False(t, compositeAccountModelOwned(unmapped, account, targetModel))
	account.Credentials = nil // An unrestricted account still cannot claim a private alias.
	require.False(t, compositeAccountModelOwned(unmapped, account, publicModel))
}
