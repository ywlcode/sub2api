package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type siteCatalogsForTest map[int64]site.CatalogGroup

func (s siteCatalogsForTest) lookup(id int64, kind string) []byte {
	group := s[id]
	switch kind {
	case site.CatalogPolicy:
		return group.Policy
	case site.CatalogPlain:
		return group.Plain
	case site.CatalogUnion:
		return group.Union
	case site.CatalogCodex:
		return group.Codex
	default:
		panic("unexpected catalog kind: " + kind)
	}
}

func buildSiteCatalogsForTest(t *testing.T, h *GatewayHandler, groups ...*service.Group) siteCatalogsForTest {
	t.Helper()
	catalogs := make(siteCatalogsForTest)
	for _, group := range groups {
		result, err := h.BuildSiteCatalog(t.Context(), group)
		require.NoError(t, err)
		require.NotEmpty(t, result.Policy)
		require.Equal(t, service.SiteCatalogGroupPolicy(group), result.Policy)
		catalogs[group.ID] = result
	}
	return catalogs
}

func catalogKeyForTest(group *service.Group, denied ...string) *service.APIKey {
	return &service.APIKey{
		ID: 17, GroupID: &group.ID, Group: group,
		User: &service.User{ID: 19, UserGroupDeniedModels: denied},
	}
}

func catalogRequestForTest(path, etag string, key *service.APIKey, groups []*service.APIKey) (*gin.Context, *httptest.ResponseRecorder) {
	c, w := newHelperTestContext(http.MethodGet, path)
	if key != nil {
		c.Set(string(middleware.ContextKeyAPIKey), key)
	}
	if groups != nil {
		c.Set("api_key.model_listing_groups", groups)
	}
	if etag != "" {
		c.Request.Header.Set("If-None-Match", etag)
	}
	return c, w
}

func (s siteCatalogsForTest) comparePrimary(t *testing.T, primary gin.HandlerFunc, path, etag string, key *service.APIKey, groups []*service.APIKey) *httptest.ResponseRecorder {
	t.Helper()
	c, want := catalogRequestForTest(path, etag, key, groups)
	primary(c)
	c, got := catalogRequestForTest(path, etag, key, groups)
	require.True(t, serveLocalModelCatalog(c, s.lookup))
	require.Equal(t, want.Code, got.Code)
	wantHeader := want.Result().Header.Clone()
	wantHeader.Set("Cache-Control", "private, no-cache")
	require.Equal(t, wantHeader, got.Result().Header)
	require.Equal(t, want.Body.String(), got.Body.String())
	return got
}

func TestSiteCatalogLeavesPrimaryAndUnsupportedRequestsUntouched(t *testing.T) {
	key := catalogKeyForTest(&service.Group{ID: 20, Platform: service.PlatformGrok})
	c, _ := catalogRequestForTest("/v1/models", "*", key, nil)
	require.False(t, (&GatewayHandler{}).ServeSiteCatalog(c), "ordinary primary dispatch")
	require.False(t, c.Writer.Written())
	require.Empty(t, c.Writer.Header())

	for _, path := range []string{"/v1/models?model=", "/models?model=one&client_version=1", "/v1/models/one?client_version=1", "/v1beta/models", "/backend-api/codex/models?future_option=1"} {
		t.Run(path, func(t *testing.T) {
			c, _ := catalogRequestForTest(path, "*", key, nil)
			require.False(t, serveLocalModelCatalog(c, func(int64, string) []byte {
				t.Fatal("unsupported request read a local catalog")
				return nil
			}))
			require.False(t, c.Writer.Written())
			require.Empty(t, c.Writer.Header())
		})
	}
}

func TestSiteCatalogRequiresPolicyAndRequestedVariant(t *testing.T) {
	key := catalogKeyForTest(&service.Group{ID: 20, Platform: service.PlatformGrok})
	for _, tc := range []struct {
		kind, path string
		groups     []*service.APIKey
	}{
		{site.CatalogPlain, "/models", nil},
		{site.CatalogUnion, "/v1/models", []*service.APIKey{key}},
		{site.CatalogCodex, "/backend-api/codex/models", nil},
	} {
		for _, missing := range []string{site.CatalogPolicy, tc.kind} {
			t.Run(tc.kind+"/missing-"+missing, func(t *testing.T) {
				c, _ := catalogRequestForTest(tc.path, "*", key, tc.groups)
				var reads []string
				require.False(t, serveLocalModelCatalog(c, func(id int64, kind string) []byte {
					require.Equal(t, key.Group.ID, id)
					reads = append(reads, kind)
					if kind == missing {
						return nil
					}
					return service.SiteCatalogGroupPolicy(key.Group)
				}))
				wantReads := []string{site.CatalogPolicy}
				if missing == tc.kind {
					wantReads = append(wantReads, tc.kind)
				}
				require.Equal(t, wantReads, reads)
				require.False(t, c.Writer.Written())
				require.Empty(t, c.Writer.Header())
			})
		}
	}
}

func TestSiteCatalogDefaultPlainIsNotAnEmptyGroupUnion(t *testing.T) {
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{})
	for _, platform := range []string{service.PlatformAnthropic, service.PlatformGemini, service.PlatformGrok, service.PlatformTypeSafe, service.PlatformComposite, service.PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			group := &service.Group{ID: 21, Platform: platform}
			key := catalogKeyForTest(group)
			catalogs := buildSiteCatalogsForTest(t, h, group)
			plain := catalogs.comparePrimary(t, h.Models, "/v1/models?client_version=", "*", key, nil)
			require.Equal(t, http.StatusOK, plain.Code)
			require.Equal(t, catalogs[group.ID].Plain, plain.Body.Bytes())
			ids := listedModelIDs(t, plain.Body.Bytes())
			require.NotEmpty(t, ids)
			key.User.UserGroupDeniedModels = []string{ids[0]}
			filtered := catalogs.comparePrimary(t, h.Models, "/models", "*", key, nil)
			require.NotContains(t, listedModelIDs(t, filtered.Body.Bytes()), ids[0])
			require.Empty(t, filtered.Header().Get("ETag"))
			require.Equal(t, "[]", string(catalogs[group.ID].Union))
			union := catalogs.comparePrimary(t, h.Models, "/v1/models", "", key, []*service.APIKey{key})
			require.Equal(t, `{"data":[],"object":"list"}`, union.Body.String())
		})
	}
}

func TestSiteCatalogGeneratedCodexPersonalizesExactBytes(t *testing.T) {
	group := &service.Group{ID: 22, Platform: service.PlatformGrok, ModelAllowlist: service.GroupModelAllowlist{
		Enabled: true, Models: []string{"z-alias", "grok-build", "grok-4.6", "codex-auto-allowed", "*"},
	}}
	account := service.Account{ID: 3, Platform: group.Platform, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"z-alias": "grok-4.6", "grok-build": "grok-build", "grok-4.6": "grok-4.6",
			"codex-auto-allowed": "grok-build", "codex-auto-hidden": "grok-build",
			"gpt-image-1": "gpt-image-1", "wild-*": "grok-build",
		}}}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{group.ID: {account}}})
	catalogs := buildSiteCatalogsForTest(t, h, group)
	key := catalogKeyForTest(group)
	all := catalogs.comparePrimary(t, h.CodexModels, "/models?client_version=0.144.0", "", key, nil)
	require.Equal(t, []string{"z-alias", "grok-build", "grok-4.6", "codex-auto-allowed"}, codexHandlerManifestSlugs(t, all))
	require.Equal(t, catalogs[group.ID].Codex, all.Body.Bytes())
	shared := all.Header().Get("ETag")
	key.User.UserGroupDeniedModels = []string{"GROK-B*", "codex-auto-*"}
	filtered := catalogs.comparePrimary(t, h.CodexModels, "/backend-api/codex/models", shared, key, nil)
	require.Equal(t, http.StatusOK, filtered.Code)
	require.Equal(t, []string{"z-alias", "grok-4.6"}, codexHandlerManifestSlugs(t, filtered))
	personal := filtered.Header().Get("ETag")
	require.Equal(t, service.CodexModelsManifestETag(filtered.Body.Bytes()), personal)
	require.NotEqual(t, shared, personal)
	cached := catalogs.comparePrimary(t, h.CodexModels, "/v1/models?client_version=other", shared+", W/"+personal, key, nil)
	require.Equal(t, http.StatusNotModified, cached.Code)
	require.Empty(t, cached.Body.Bytes())
	key.User.UserGroupDeniedModels = []string{"*"}
	empty := catalogs.comparePrimary(t, h.CodexModels, "/backend-api/codex/models", personal, key, nil)
	require.Equal(t, `{"models":[]}`, empty.Body.String())
	require.Equal(t, service.CodexModelsManifestETag(empty.Body.Bytes()), empty.Header().Get("ETag"))
}

func TestSiteCatalogOpenAIConfiguredCodexMatchesPrimary(t *testing.T) {
	group := &service.Group{ID: 23, Platform: service.PlatformOpenAI}
	account := newPinnedCodexAccount(4, service.StatusActive, true, false)
	account.Credentials["model_mapping"] = map[string]any{"public-z": "gpt-6.1-sol", "public-a": "gpt-6-astra"}
	upstream := &codexModelsPinnedHTTPUpstream{}
	codex := newPinnedCodexTestHandler([]service.Account{account}, upstream, 3)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{group.ID: {account}}})
	h.openAIGatewayService = codex.gatewayService
	catalogs := buildSiteCatalogsForTest(t, h, group)
	key := catalogKeyForTest(group)
	path := "/v1/models?client_version=0.144.0"
	all := catalogs.comparePrimary(t, codex.CodexModels, path, "", key, nil)
	require.Equal(t, []string{"public-a", "public-z"}, codexHandlerManifestSlugs(t, all))
	require.Equal(t, all.Body.Bytes(), catalogs[group.ID].Codex)
	etag := all.Header().Get("ETag")
	require.Equal(t, service.CodexModelsManifestETag(all.Body.Bytes()), etag)
	cached := catalogs.comparePrimary(t, codex.CodexModels, "/backend-api/codex/models", "W/"+etag, key, nil)
	require.Equal(t, http.StatusNotModified, cached.Code)
	require.Empty(t, cached.Body.Bytes())

	for _, tc := range []struct {
		deny string
		want []string
	}{
		{"PUBLIC-A", []string{"public-z"}},
		{"absent", []string{"public-a", "public-z"}},
		{"*", []string{}},
	} {
		key.User.UserGroupDeniedModels = []string{tc.deny}
		filtered := catalogs.comparePrimary(t, codex.CodexModels, path, "*", key, nil)
		require.Equal(t, http.StatusOK, filtered.Code)
		require.Equal(t, tc.want, codexHandlerManifestSlugs(t, filtered))
		require.Empty(t, filtered.Header().Get("ETag"))
	}
	require.Empty(t, upstream.accountIDs(), "neither export nor configured discovery may contact the upstream")
}

func TestSiteCatalogPinnedAndLiveNeverFetchUpstream(t *testing.T) {
	group := &service.Group{ID: 24, Platform: service.PlatformOpenAI}
	account := newPinnedCodexAccount(5, service.StatusActive, true, false)
	upstream := &codexModelsPinnedHTTPUpstream{}
	codex := newPinnedCodexTestHandler([]service.Account{account}, upstream, 3)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{group.ID: {account}}})
	h.openAIGatewayService = codex.gatewayService
	key := catalogKeyForTest(group)
	live := buildSiteCatalogsForTest(t, h, group)
	require.NotNil(t, live[group.ID].Plain)
	require.NotNil(t, live[group.ID].Union)
	require.Nil(t, live[group.ID].Codex)
	live.comparePrimary(t, h.Models, "/models", "", key, nil)
	c, _ := catalogRequestForTest("/backend-api/codex/models?client_version=1", "", key, nil)
	require.False(t, serveLocalModelCatalog(c, live.lookup))

	group.CodexModelsManifestConfig = service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{account.ID}, FallbackToScheduler: true}
	pinned := buildSiteCatalogsForTest(t, h, group)
	require.Nil(t, pinned[group.ID].Plain)
	require.Nil(t, pinned[group.ID].Union)
	require.Nil(t, pinned[group.ID].Codex)
	require.Empty(t, upstream.accountIDs())
}

func TestSiteCatalogUnionPerGroupDenyAndDuplicateMetadata(t *testing.T) {
	lower := &service.Group{ID: 25, Platform: service.PlatformOpenAI}
	higher := &service.Group{ID: 26, Platform: service.PlatformGrok}
	account := func(group *service.Group, unique string) service.Account {
		return service.Account{ID: group.ID, Platform: group.Platform, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{unique: unique, "shared": "shared"}}}
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		lower.ID: {account(lower, "text")}, higher.ID: {account(higher, "image")},
	}})
	catalogs := buildSiteCatalogsForTest(t, h, lower, higher)
	lo, hi := catalogKeyForTest(lower), catalogKeyForTest(higher, "image")
	hi.GroupIDs = []int64{higher.ID, lower.ID}
	groups := []*service.APIKey{hi, lo}
	for _, owner := range []string{"openai", "xai"} {
		if owner == "xai" {
			lo.User.UserGroupDeniedModels = []string{"SHAR*"}
		}
		result := catalogs.comparePrimary(t, h.Models, "/v1/models", "*", hi, groups)
		require.Equal(t, []string{"shared", "text"}, listedModelIDs(t, result.Body.Bytes()))
		var entries gatewayModelsResponseForTest
		require.NoError(t, json.Unmarshal(result.Body.Bytes(), &entries))
		require.Equal(t, owner, entries.Data[0].OwnedBy)
		require.Empty(t, result.Header().Get("ETag"))
		require.Same(t, hi, groups[0])
	}

	// The second candidate changed after publication. No partial union (or
	// representative-only response) can be committed before forwarding.
	higher.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"image"}}
	c, _ := catalogRequestForTest("/models", "", lo, groups)
	read := func(id int64, kind string) []byte {
		if id == higher.ID {
			require.Equal(t, site.CatalogPolicy, kind, "must never read bytes under the old policy")
		}
		return catalogs.lookup(id, kind)
	}
	require.False(t, serveLocalModelCatalog(c, read))
	require.False(t, c.Writer.Written())
}

type failingSiteCatalogRepo struct{ gatewayModelsAccountRepoStub }

func (*failingSiteCatalogRepo) ListSchedulableByGroupID(context.Context, int64) ([]service.Account, error) {
	return nil, errors.New("catalog repository unavailable")
}

func TestSiteCatalogBuildFailureCannotPublishDefaults(t *testing.T) {
	h := newGatewayModelsHandlerForTest(&failingSiteCatalogRepo{})
	group := &service.Group{ID: 27, Platform: service.PlatformAnthropic}
	result, err := h.BuildSiteCatalog(t.Context(), group)
	require.ErrorContains(t, err, "catalog repository unavailable")
	require.Empty(t, result)
}
