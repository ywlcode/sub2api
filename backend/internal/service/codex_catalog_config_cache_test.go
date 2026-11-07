package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type catalogConfigRepo struct {
	AccountRepository
	accounts     []Account
	reads        atomic.Int32
	visibleReads atomic.Int32
	err          error
}

func (r *catalogConfigRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	r.reads.Add(1)
	return append([]Account(nil), r.accounts...), r.err
}
func (r *catalogConfigRepo) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	r.visibleReads.Add(1)
	return append([]Account(nil), r.accounts...), nil
}

func TestCodexCatalogConfigCacheSkipsReadsAndSeparatesPermissions(t *testing.T) {
	ctx := context.Background()
	repo := &catalogConfigRepo{accounts: []Account{{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "deepseek-chat", "private": "deepseek-reasoner"}}}}}
	group := &Group{ID: 1, Platform: PlatformComposite}
	routes := &countedRouteConfigRepo{}
	gateway := &GatewayService{accountRepo: repo, compositeResolver: NewCompositeRouteResolver(routes)}
	auth := &APIKeyService{cache: &requestConfigAuthCacheStub{}}
	auth.SetGroupRouter(gateway)
	auth.authInvalidationConnected.Store(true)
	for range 3 {
		got, err := gateway.BuildCodexModelsManifestForGroup(ctx, group, "", []string{"alias"})
		require.NoError(t, err)
		want, err := buildCodexModelsManifestForAccounts(group.Platform, []string{"alias"}, repo.accounts, group, nil, true)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
		got[0] = 'X'
		require.Contains(t, gateway.GetSchedulablePlatforms(ctx, &group.ID), PlatformDeepseek)
	}
	require.Equal(t, int32(1), repo.reads.Load())
	require.Equal(t, int32(1), routes.calls.Load())
	require.Equal(t, int32(1), repo.visibleReads.Load())
	wider, err := gateway.BuildCodexModelsManifestForGroup(ctx, group, "", []string{"alias", "private"})
	require.NoError(t, err)
	require.Contains(t, string(wider), `"private"`)
	restricted, err := gateway.BuildCodexModelsManifestForGroup(ctx, group, "", []string{"alias"})
	require.NoError(t, err)
	require.NotContains(t, string(restricted), `"private"`)
	require.Equal(t, int32(2), repo.reads.Load())
	repo.accounts[0].Credentials["model_mapping"] = map[string]any{"alias": "deepseek-reasoner"}
	auth.applyRequestConfigInvalidation(RequestConfigInvalidationPrefix + "catalog:0")
	got, err := gateway.BuildCodexModelsManifestForGroup(ctx, group, "", []string{"alias"})
	require.NoError(t, err)
	want, err := buildCodexModelsManifestForAccounts(group.Platform, []string{"alias"}, repo.accounts, group, nil, true)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
	require.Equal(t, int32(3), repo.reads.Load())
}

func TestCodexCatalogConfigCacheDoesNotStoreDegradedFallback(t *testing.T) {
	repo := &catalogConfigRepo{err: errors.New("availability unavailable")}
	gateway := &GatewayService{accountRepo: repo, configCacheReady: func() bool { return true }}
	group := &Group{ID: 1, Platform: PlatformDeepseek}
	for range 2 {
		_, err := gateway.BuildCodexModelsManifestForGroup(context.Background(), group, "", []string{"deepseek-chat"})
		require.NoError(t, err)
		require.Empty(t, gateway.codexCatalogResponseCache.entries)
	}
	repo.err = nil
	for range 2 {
		_, err := gateway.BuildCodexModelsManifestForGroup(context.Background(), group, "", []string{"deepseek-chat"})
		require.NoError(t, err)
	}
	require.Equal(t, int32(5), repo.reads.Load(), "two uncached fallbacks, then a successful fill and hit")
}
