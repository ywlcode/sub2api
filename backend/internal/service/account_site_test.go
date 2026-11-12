package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecondaryAccountPermissionIsAdditional(t *testing.T) {
	a := Account{Type: AccountTypeAPIKey, Platform: PlatformDeepseek, Status: StatusActive, Schedulable: true}
	require.False(t, a.SecondaryEnabled())
	for _, enabled := range []bool{true, false} {
		a.Extra = map[string]any{AccountExtraSecondaryEnabled: enabled}
		require.Equal(t, enabled, a.SecondaryEnabled())
		require.True(t, a.IsSchedulable(), "primary scheduling must be independent of secondary permission")
		require.True(t, a.IsModelSupported("deepseek-flash"))
	}
	for _, typ := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, shadow := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				err := ValidateSecondaryAccountExtra(typ, shadow, map[string]any{AccountExtraSecondaryEnabled: enabled})
				if enabled && (typ != AccountTypeAPIKey || shadow) {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
		}
	}
	for _, value := range []any{"true", 1, nil} {
		require.Error(t, ValidateSecondaryAccountExtra(AccountTypeAPIKey, false, map[string]any{AccountExtraSecondaryEnabled: value}))
	}
}

func TestSiteModelRoutingUsesAccountPermissionsAndExistingModelRules(t *testing.T) {
	groupID := int64(1)
	groups := []Group{{ID: groupID, Platform: PlatformComposite, Status: StatusActive}, {ID: 2, Platform: PlatformComposite, Status: StatusActive}}
	keys := []APIKey{{Key: "digest", GroupID: &groupID, ModelMapping: map[string]string{"my-key-alias": "deepseek-flash"}}, {Key: "other-group", GroupIDs: []int64{2}}}
	a := Account{ID: 54, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Extra: map[string]any{AccountExtraSecondaryEnabled: true}}
	router := func(accounts []Account, routes []CompositeModelRoute) *SiteModelRouter {
		return NewSiteModelRouter(accounts, routes, groups, keys)
	}
	r := router([]Account{a}, nil)
	for _, model := range []string{"deepseek-flash", "deepseek-flash[1m]", "my-key-alias"} {
		require.True(t, r.Match("digest", model, "responses"), model)
	}
	require.False(t, r.Match("digest", "gpt-5", "responses"))
	require.False(t, r.Match("other-group", "deepseek-flash", "responses"))
	require.False(t, r.Match("unknown-key", "deepseek-flash", "responses"))
	a.Credentials = map[string]any{"model_mapping": map[string]any{"custom-*": "deepseek-flash", "alias": "deepseek-flash"}}
	r = router([]Account{a}, nil)
	require.True(t, r.Match("digest", "custom-small", "responses"))
	require.True(t, r.Match("digest", "alias", "responses"))
	require.False(t, r.Match("digest", "deepseek-flash", "responses"))
	a.AccountGroups = []AccountGroup{{GroupID: groupID, AllowedModels: []string{"alias"}}}
	r = router([]Account{a}, nil)
	require.False(t, r.Match("digest", "custom-small", "responses"))
	require.True(t, r.Match("digest", "alias", "responses"))
	a.Extra = map[string]any{AccountExtraSecondaryEnabled: false}
	require.False(t, router([]Account{a}, nil).Match("digest", "alias", "responses"))
}

func TestSiteCompositeRoutePrecedenceAndPlatformIsolation(t *testing.T) {
	groupID := int64(1)
	a := Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Extra: map[string]any{AccountExtraSecondaryEnabled: true}}
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: 1, PublicModel: "shared", MatchType: CompositeRouteMatchPrefix, TargetPlatform: PlatformDeepseek, UpstreamModel: "deepseek-flash", Enabled: true},
		{ID: 2, GroupID: 1, PublicModel: "shared-gpt", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5", Enabled: true},
		{ID: 3, GroupID: 1, PublicModel: "deepseek-flash", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5", Endpoint: "messages", Enabled: true},
	}
	groups := []Group{{ID: 1, Platform: PlatformComposite, Status: StatusActive}}
	keys := []APIKey{{Key: "key", GroupID: &groupID}}
	r := NewSiteModelRouter([]Account{a}, routes, groups, keys)
	require.True(t, r.Match("key", "shared-ds", "responses"))
	require.False(t, r.Match("key", "shared-gpt", "responses"))
	require.False(t, r.Match("key", "deepseek-flash", "messages"))
	require.True(t, r.Match("key", "deepseek-flash", "responses"))
	// An unrestricted provider mapping must not absorb another platform's models.
	a.Platform = PlatformKimi
	r = NewSiteModelRouter([]Account{a}, nil, groups, keys)
	require.False(t, r.Match("key", "gpt-5", "responses"))
}

func TestSiteChannelAliasUsesSameMappingAsGateway(t *testing.T) {
	groupID := int64(1)
	account := Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{1}, Extra: map[string]any{AccountExtraSecondaryEnabled: true}, Credentials: map[string]any{"model_mapping": map[string]any{"deepseek-chat": "deepseek-chat"}}}
	router := NewSiteModelRouter([]Account{account}, nil, []Group{{ID: 1, Platform: PlatformComposite, Status: StatusActive}}, []APIKey{{Key: "key", GroupID: &groupID}}, func(id int64, model string) string {
		if id == 1 && model == "office-alias" {
			return "deepseek-chat"
		}
		return model
	})
	require.True(t, router.Match("key", "office-alias", "responses"))
	require.False(t, router.Match("key", "gpt-5", "responses"))
}
