package service

import (
	"context"
	"slices"
	"sort"
	"strings"
)

type compositeEndpointRoutes struct {
	exact    map[string]CompositeModelRoute
	prefixes []CompositeModelRoute
}

type compositeRouteTable struct {
	routes    []CompositeModelRoute
	endpoints map[string]compositeEndpointRoutes
}

func compileCompositeRoutes(routes []CompositeModelRoute) *compositeRouteTable {
	table := &compositeRouteTable{routes: slices.Clone(routes), endpoints: make(map[string]compositeEndpointRoutes)}
	for _, endpoint := range []string{CompositeRouteEndpointAny, CompositeRouteEndpointMessages,
		CompositeRouteEndpointCountTokens, CompositeRouteEndpointResponses, CompositeRouteEndpointChatCompletions,
		CompositeRouteEndpointEmbeddings, CompositeRouteEndpointImages, CompositeRouteEndpointGemini} {
		candidates := make([]CompositeModelRoute, 0, len(routes))
		for _, route := range routes {
			route.Endpoint = normalizeCompositeRouteEndpoint(route.Endpoint)
			route.MatchType = normalizeCompositeRouteMatchType(route.MatchType)
			if route.Endpoint != endpoint && route.Endpoint != CompositeRouteEndpointAny {
				continue
			}
			if strings.TrimSpace(route.PublicModel) == "" {
				continue
			}
			candidates = append(candidates, route)
		}
		// Same precedence as matchCompositeRoute, calculated once per configuration.
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.MatchType != b.MatchType {
				return a.MatchType == CompositeRouteMatchExact
			}
			if (a.Endpoint == endpoint) != (b.Endpoint == endpoint) {
				return a.Endpoint == endpoint
			}
			if len(strings.TrimSpace(a.PublicModel)) != len(strings.TrimSpace(b.PublicModel)) {
				return len(strings.TrimSpace(a.PublicModel)) > len(strings.TrimSpace(b.PublicModel))
			}
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			return a.ID < b.ID
		})
		index := compositeEndpointRoutes{exact: make(map[string]CompositeModelRoute)}
		for _, route := range candidates {
			model := strings.TrimSpace(route.PublicModel)
			if route.MatchType == CompositeRouteMatchExact {
				if _, found := index.exact[model]; !found {
					index.exact[model] = route
				}
			} else {
				index.prefixes = append(index.prefixes, route)
			}
		}
		table.endpoints[endpoint] = index
	}
	return table
}

func (t *compositeRouteTable) match(model, endpoint string) (CompositeModelRoute, bool) {
	index := t.endpoints[normalizeCompositeRouteEndpoint(endpoint)]
	if route, found := index.exact[model]; found {
		return route, true
	}
	for _, route := range index.prefixes {
		if strings.HasPrefix(model, strings.TrimSpace(route.PublicModel)) {
			return route, true
		}
	}
	return CompositeModelRoute{}, false
}

func (r *CompositeRouteResolver) cachedRouteTable(ctx context.Context, groupID int64) (*compositeRouteTable, error) {
	load := func(ctx context.Context) (*compositeRouteTable, int, error) {
		routes, err := r.repo.ListByGroup(ctx, groupID, false)
		if err != nil {
			return nil, 0, err
		}
		cost := 256
		for _, route := range routes {
			cost += 2048 + len(route.PublicModel)*10 + len(route.UpstreamModel) + len(route.Notes)
		}
		return compileCompositeRoutes(routes), cost, nil
	}
	if r.configCacheReady == nil || !r.configCacheReady() {
		value, _, err := load(ctx)
		return value, err
	}
	return r.routeCache.get(ctx, groupID, load)
}
