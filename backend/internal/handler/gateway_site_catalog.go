package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
)

const siteCatalogCacheControl = "private, no-cache"

// BuildSiteCatalog exports the same unpersonalized representations used by the
// primary. Only the configured branch of OpenAI Codex discovery is exportable.
func (h *GatewayHandler) BuildSiteCatalog(ctx context.Context, group *service.Group) (site.CatalogGroup, error) {
	catalog := site.CatalogGroup{Policy: service.SiteCatalogGroupPolicy(group)}
	if !generatesSiteCatalog(group) {
		return catalog, nil
	}
	if h == nil || h.gatewayService == nil {
		return site.CatalogGroup{}, errors.New("model discovery is not configured")
	}

	// Run the strict repository read first. A failed multi-group lookup must
	// never be published as a successful empty list or a legacy default list.
	var err error
	catalog.Union, err = h.generatedUnionCatalog(ctx, group, group.Platform)
	if err != nil {
		return site.CatalogGroup{}, err
	}
	catalog.Plain, err = json.Marshal(modelsListEnvelope(h.plainModelEntries(ctx, group, group.Platform)))
	if err != nil {
		return site.CatalogGroup{}, err
	}
	catalog.Codex, err = h.siteCodexManifest(ctx, group)
	if err != nil {
		return site.CatalogGroup{}, err
	}
	if err := ctx.Err(); err != nil {
		return site.CatalogGroup{}, err
	}
	return catalog, nil
}

func (h *GatewayHandler) siteCodexManifest(ctx context.Context, group *service.Group) ([]byte, error) {
	if group.Platform != service.PlatformOpenAI {
		return h.generatedCodexManifest(ctx, group, "", nil)
	}
	if h.openAIGatewayService == nil {
		return nil, nil
	}
	manifest, configured, err := h.openAIGatewayService.BuildGroupConfiguredCodexModelsManifest(ctx, group, "")
	if err != nil || !configured {
		return nil, err
	}
	return manifest.Body, nil
}

func generatesSiteCatalog(group *service.Group) bool {
	return group != nil && group.ID > 0 && domain.IsGroupPlatform(group.Platform) && !usesPinnedModelCatalog(group)
}

// ServeSiteCatalog runs after authentication and before model-handler dispatch.
// A discovery request is handled exactly once, including a replay at the primary.
func (h *GatewayHandler) ServeSiteCatalog(c *gin.Context) bool {
	lookup := site.DiscoveryFrom(c)
	if lookup == nil {
		return false
	}
	if !serveLocalModelCatalog(c, lookup) {
		site.ForwardDiscovery(c)
	}
	return true
}

func serveLocalModelCatalog(c *gin.Context, lookup func(int64, string) []byte) bool {
	key, _ := middleware.GetAPIKeyFromContext(c)
	// Slot ownership, queued revalidation and IP ACLs belong to the primary.
	if !key.CanServeSiteDiscovery() || key.Group == nil || middleware.HasForcePlatform(c) || c.Param("model") != "" {
		return false
	}
	if _, resolved := service.ResolvedTargetPlatformFromContext(c.Request.Context()); resolved {
		return false
	}
	kind := site.DiscoveryKind(c.Request)
	if kind == "" {
		return false
	}

	if groups, union := middleware.GetModelListingGroups(c); union {
		if kind != site.CatalogPlain || len(groups) == 0 {
			return false
		}
		models, err := mergeModelCatalogs(groups, func(candidate *service.APIKey) ([]json.RawMessage, error) {
			body := siteCatalogForKey(lookup, candidate, site.CatalogUnion)
			return decodeUnionCatalog(body)
		})
		if err != nil {
			return false
		}
		c.Header("Cache-Control", siteCatalogCacheControl)
		c.JSON(http.StatusOK, modelsListEnvelope(models))
		return true
	}
	// An ordinary multi-group collection needs all authorized candidates.
	// A Codex manifest uses only the group selected by authentication.
	if kind == site.CatalogPlain && len(key.GroupIDs) > 1 {
		return false
	}
	body := siteCatalogForKey(lookup, key, kind)
	if len(body) == 0 {
		return false
	}
	if kind == site.CatalogCodex && key.Group.Platform == service.PlatformOpenAI {
		// The configured OpenAI handler suppresses the shared ETag for every
		// user with denied models, even when none of those slugs is present.
		etag := service.CodexModelsManifestETag(body)
		c.Header("Cache-Control", siteCatalogCacheControl)
		writeOpenAIModelsResponse(c, &service.OpenAIModelsResponse{
			Body:        body,
			ETag:        etag,
			NotModified: service.CodexModelsManifestETagMatches(modelsIfNoneMatch(c), etag),
		})
		return true
	}
	if denied := key.DeniedModelsInGroup(); len(denied) > 0 {
		// A generated descriptor depends only on its public slug and group
		// configuration. Keeping the original entries in order reproduces
		// primary generation with the denied slugs removed from its input.
		var err error
		body, err = removeDeniedCatalogEntries(body, denied)
		if err != nil {
			return false
		}
	}
	c.Header("Cache-Control", siteCatalogCacheControl)
	if kind == site.CatalogCodex {
		writeGeneratedCodexManifest(c, body)
	} else {
		writePlainModelsBody(c, body)
	}
	return true
}

func siteCatalogForKey(lookup func(int64, string) []byte, key *service.APIKey, kind string) []byte {
	if !key.CanServeSiteDiscovery() || !generatesSiteCatalog(key.Group) {
		return nil
	}
	group := key.Group
	policy := service.SiteCatalogGroupPolicy(group)
	if len(policy) == 0 || !bytes.Equal(lookup(group.ID, site.CatalogPolicy), policy) {
		return nil
	}
	return lookup(group.ID, kind)
}
