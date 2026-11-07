package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type countedRouteConfigRepo struct {
	CompositeModelRouteRepository
	routes []CompositeModelRoute
	calls  atomic.Int32
	err    error
}

func (r *countedRouteConfigRepo) ListByGroup(context.Context, int64, bool) ([]CompositeModelRoute, error) {
	r.calls.Add(1)
	return r.routes, r.err
}

func TestCompositeRouteCacheMatchesOriginalPrecedence(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	endpoints := []string{"any", "responses", "messages", "chat_completions", "images", "embeddings", "gemini", "count_tokens"}
	for round := 0; round < 200; round++ {
		routes := make([]CompositeModelRoute, 40)
		for i := range routes {
			kind := "exact"
			if rng.Intn(2) == 0 {
				kind = "prefix"
			}
			model := []string{"gpt", "gpt-5", "gpt-5.4", "deepseek", " deepseek-x ", ""}[rng.Intn(6)]
			routes[i] = CompositeModelRoute{ID: int64(i), PublicModel: model, MatchType: kind, Endpoint: endpoints[rng.Intn(len(endpoints))], Priority: rng.Intn(4), UpstreamModel: fmt.Sprint(i), Enabled: true}
		}
		table := compileCompositeRoutes(routes)
		for _, endpoint := range endpoints {
			for _, model := range []string{"gpt", "gpt-5", "gpt-5.4", "gpt-5.4-mini", "deepseek-x", "unknown"} {
				want, ok := matchCompositeRoute(routes, model, endpoint)
				got, found := table.match(model, endpoint)
				require.Equal(t, ok, found)
				require.Equal(t, want, got)
			}
		}
	}
}

func TestCompositeRouteCacheInvalidationEmptyAndRecovery(t *testing.T) {
	repo := &countedRouteConfigRepo{}
	resolver := NewCompositeRouteResolver(repo)
	resolver.configCacheReady = func() bool { return true }
	ctx := context.Background()
	for range 3 {
		_, err := resolver.Resolve(ctx, 1, "gpt-5", "responses")
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), repo.calls.Load())
	repo.routes = []CompositeModelRoute{{GroupID: 1, PublicModel: "gpt-5", MatchType: "exact", TargetPlatform: PlatformDeepseek, UpstreamModel: "deepseek-x", Endpoint: "any", Enabled: true}}
	resolver.routeCache.invalidate(nil)
	result, err := resolver.Resolve(ctx, 1, "gpt-5", "responses")
	require.NoError(t, err)
	require.Equal(t, "deepseek-x", result.UpstreamModel)
	result.Route.UpstreamModel = "corrupt"
	result, err = resolver.Resolve(ctx, 1, "gpt-5", "responses")
	require.NoError(t, err)
	require.Equal(t, "deepseek-x", result.UpstreamModel)
	repo.err = errors.New("offline")
	resolver.routeCache.invalidate(nil)
	_, err = resolver.Resolve(ctx, 1, "gpt-5", "responses")
	require.Error(t, err)
	repo.err = nil
	_, err = resolver.Resolve(ctx, 1, "gpt-5", "responses")
	require.NoError(t, err)
	require.Equal(t, int32(4), repo.calls.Load())
}

func (r *countedRouteConfigRepo) Create(_ context.Context, route *CompositeModelRoute) error {
	route.ID = 1
	r.routes = append(r.routes, *route)
	return nil
}
func (r *countedRouteConfigRepo) Update(_ context.Context, route *CompositeModelRoute) error {
	for i := range r.routes {
		if r.routes[i].ID == route.ID {
			r.routes[i] = *route
		}
	}
	return nil
}
func (r *countedRouteConfigRepo) Delete(_ context.Context, id int64) error {
	for i := range r.routes {
		if r.routes[i].ID == id {
			r.routes = append(r.routes[:i], r.routes[i+1:]...)
			break
		}
	}
	return nil
}

func TestCompositeRouteCacheAdminEditsInvalidate(t *testing.T) {
	ctx := context.Background()
	repo := &countedRouteConfigRepo{}
	resolver := NewCompositeRouteResolver(repo)
	resolver.configCacheReady = func() bool { return true }
	admin := &adminServiceImpl{compositeRouteRepo: repo, compositeResolver: resolver, groupRepo: multiGroupRepoStub{groups: map[int64]*Group{1: {ID: 1, Platform: PlatformComposite}}}}
	models, err := resolver.ListExactPublicModels(ctx, 1, "responses")
	require.NoError(t, err)
	require.Empty(t, models)
	input := CompositeRouteInput{PublicModel: "alias", MatchType: "exact", TargetPlatform: PlatformDeepseek, UpstreamModel: "deepseek-chat", Endpoint: "any", Enabled: true}
	route, err := admin.CreateCompositeRoute(ctx, 1, input)
	require.NoError(t, err)
	decision, err := resolver.Resolve(ctx, 1, "alias", "responses")
	require.NoError(t, err)
	require.Equal(t, "deepseek-chat", decision.UpstreamModel)
	input.UpstreamModel = "deepseek-reasoner"
	_, err = admin.UpdateCompositeRoute(ctx, 1, route.ID, input)
	require.NoError(t, err)
	decision, err = resolver.Resolve(ctx, 1, "alias", "responses")
	require.NoError(t, err)
	require.Equal(t, "deepseek-reasoner", decision.UpstreamModel)
	require.NoError(t, admin.DeleteCompositeRoute(ctx, 1, route.ID))
	decision, err = resolver.Resolve(ctx, 1, "alias", "responses")
	require.NoError(t, err)
	require.False(t, decision.Matched)
}
