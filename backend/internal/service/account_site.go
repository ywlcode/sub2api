package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const AccountExtraSecondaryEnabled = "site_secondary_enabled"

// SecondaryEnabled grants an additional serving location. Primary scheduling
// does not consult this flag and continues to use the account normally.
func (a *Account) SecondaryEnabled() bool {
	if a == nil || a.Type != AccountTypeAPIKey || a.ParentAccountID != nil {
		return false
	}
	enabled, _ := a.Extra[AccountExtraSecondaryEnabled].(bool)
	return enabled
}

func ValidateSecondaryAccountExtra(accountType string, shadow bool, extra map[string]any) error {
	value, present := extra[AccountExtraSecondaryEnabled]
	if !present {
		return nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return infraerrors.BadRequest("INVALID_SITE_SECONDARY_ENABLED", "site_secondary_enabled must be a boolean")
	}
	if enabled && (accountType != AccountTypeAPIKey || shadow) {
		return infraerrors.BadRequest("INVALID_SITE_SECONDARY_ACCOUNT", "secondary processing requires an account with its own API key")
	}
	return nil
}

// SiteModelRouter reuses account model matching, including default models,
// aliases and wildcards. It is only a location hint: the normal gateway still
// applies authentication, group permissions, billing and account scheduling.
// Published instances are immutable; no primary request or RPC is needed.
type SiteModelRouter struct {
	accounts   map[int64][]Account
	routes     map[int64]*compositeRouteTable
	groups     map[int64]Group
	keys       map[string]APIKey
	mapChannel func(int64, string) string
}

func NewSiteModelRouter(accounts []Account, routes []CompositeModelRoute, groups []Group, keys []APIKey, channelMapping ...func(int64, string) string) *SiteModelRouter {
	r := &SiteModelRouter{accounts: make(map[int64][]Account), routes: make(map[int64]*compositeRouteTable), groups: make(map[int64]Group), keys: make(map[string]APIKey)}
	if len(channelMapping) > 0 {
		r.mapChannel = channelMapping[0]
	}
	for _, a := range accounts {
		if a.SecondaryEnabled() && a.Status == StatusActive && a.Schedulable {
			a.PrepareSharedSnapshot()
			for _, id := range a.GroupIDs {
				r.accounts[id] = append(r.accounts[id], a)
			}
		}
	}
	byGroup := make(map[int64][]CompositeModelRoute)
	for _, route := range routes {
		if route.Enabled {
			byGroup[route.GroupID] = append(byGroup[route.GroupID], route)
		}
	}
	for id, routes := range byGroup {
		r.routes[id] = compileCompositeRoutes(routes)
	}
	for _, group := range groups {
		r.groups[group.ID] = group
	}
	for _, key := range keys {
		r.keys[key.Key] = key
	}
	return r
}

func (r *SiteModelRouter) Match(keyDigest, model, endpoint string) bool {
	if r == nil || strings.TrimSpace(model) == "" {
		return false
	}
	key, ok := r.keys[keyDigest]
	if !ok {
		return false
	}
	switch endpoint {
	case CompositeRouteEndpointMessages, CompositeRouteEndpointCountTokens, CompositeRouteEndpointChatCompletions, CompositeRouteEndpointResponses:
		model = MapAPIKeyModel(&key, model)
	}
	for _, groupID := range key.ConfiguredGroupIDs() {
		group, ok := r.groups[groupID]
		if !ok || !group.IsActive() {
			continue
		}
		groupModel := model
		if r.mapChannel != nil {
			groupModel = r.mapChannel(groupID, model)
		}
		platform, mapped := group.Platform, groupModel
		accounts := r.accounts[groupID]
		if platform == PlatformComposite {
			platform, _ = DetectModelPlatform(groupModel)
			// Explicit account aliases take precedence over the model prefix,
			// exactly as in the existing composite resolver.
			owner := ""
			ambiguous := false
			for _, a := range accounts {
				if explicitModelMappingClaims(a, groupModel) {
					if owner != "" && owner != a.Platform {
						ambiguous = true
					}
					owner = a.Platform
				}
			}
			if owner != "" && !ambiguous {
				platform = owner
			}
			if table := r.routes[groupID]; table != nil {
				if route, ok := table.match(groupModel, endpoint); ok {
					platform = route.TargetPlatform
					if strings.TrimSpace(route.UpstreamModel) != "" {
						mapped = strings.TrimSpace(route.UpstreamModel)
					}
				}
			}
		}
		for i := range accounts {
			a := &accounts[i]
			if (a.Platform == platform || platform == "") && a.IsModelSupportedInGroup(&groupID, mapped) {
				return true
			}
		}
	}
	return false
}
