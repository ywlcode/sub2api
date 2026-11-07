package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
)

var errCodexCatalogIncomplete = errors.New("catalog configuration unavailable")

// Cache the final descriptor before loading repository configuration. The key
// still includes the caller's effective models, group policy and runtime model
// mapping version; permission filtering remains outside this cache. No account
// credentials or mutable account objects are retained.
func (s *GatewayService) cachedCodexCatalogResponse(ctx context.Context, group *Group, platform string, models []string) ([]byte, error) {
	key, err := groupManifestKey(platform, models, nil, group, nil, true)
	if err != nil {
		return nil, err
	}
	body, err := s.codexCatalogResponseCache.get(ctx, key, func(ctx context.Context) ([]byte, int, error) {
		catalog, err := loadCodexCatalogCandidates(ctx, s.accountRepo, group.ID)
		if err != nil {
			return nil, 0, errCodexCatalogIncomplete
		}
		var routes []CompositeModelRoute
		if platform == PlatformComposite && s.compositeResolver != nil && s.compositeResolver.repo != nil {
			table, err := s.compositeResolver.cachedRouteTable(ctx, group.ID)
			if err != nil {
				return nil, 0, errCodexCatalogIncomplete
			}
			routes = table.routes
		}
		body, err := buildCodexModelsManifestForAccounts(platform, models, catalog, group, routes, true)
		return body, len(body), err
	})
	return bytes.Clone(body), err
}

func loadCodexCatalogCandidates(ctx context.Context, repo AccountRepository, groupID int64) ([]Account, error) {
	return repo.ListModelAvailabilityCandidates(ctx, &groupID, []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity,
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
		PlatformOpenCodeGo, PlatformMistral,
	}, false)
}

func (s *GatewayService) cachedCatalogPlatforms(ctx context.Context, groupID int64) (map[string]struct{}, error) {
	values, err := s.catalogPlatformsCache.get(ctx, groupID, func(ctx context.Context) ([]string, int, error) {
		accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, groupID)
		if err != nil {
			return nil, 0, err
		}
		seen := make(map[string]struct{})
		for _, account := range accounts {
			if platform := strings.TrimSpace(account.Platform); platform != "" {
				seen[platform] = struct{}{}
			}
		}
		values := make([]string, 0, len(seen))
		for platform := range seen {
			values = append(values, platform)
		}
		return values, 64 + len(values)*32, nil
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(values))
	for _, platform := range values {
		result[platform] = struct{}{}
	}
	return result, nil
}
