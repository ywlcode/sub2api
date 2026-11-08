package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Namespaced messages share the existing authenticated Redis invalidation
// subscription. They contain identifiers only, never configuration or secrets.
const RequestConfigInvalidationPrefix = "request-config:v1:"

func (s *APIKeyService) applyRequestConfigInvalidation(message string) bool {
	if !strings.HasPrefix(message, RequestConfigInvalidationPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(message, RequestConfigInvalidationPrefix), ":")
	if len(parts) != 2 {
		return true
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return true
	}
	kind := parts[0]
	switch kind {
	case "all":
		s.invalidateSecurityPenaltyConfig()
		s.requestGroups.invalidate(nil)
		s.requestPolicies.invalidate(nil)
		s.requestRPMOverrides.invalidate(nil)
	case "group":
		s.requestGroups.invalidate(func(key int64) bool { return key == id })
		s.requestPolicies.invalidate(func(key userGroupPolicyKey) bool { return key.groupID == id })
		s.requestRPMOverrides.invalidate(func(key userGroupPolicyKey) bool { return key.groupID == id })
	case "user":
		s.requestPolicies.invalidate(func(key userGroupPolicyKey) bool { return key.userID == id })
		s.requestRPMOverrides.invalidate(func(key userGroupPolicyKey) bool { return key.userID == id })
	case "catalog":
	default:
		return true
	}
	if gateway := s.requestConfigGateway.Load(); gateway != nil && kind != "user" {
		gateway.InvalidateAvailableModelsCache(nil, "")
		if gateway.compositeResolver != nil && (kind == "all" || kind == "group") {
			if kind == "all" {
				gateway.compositeResolver.routeCache.invalidate(nil)
			} else {
				gateway.compositeResolver.routeCache.invalidate(func(key int64) bool { return key == id })
			}
		}
	}
	return true
}

func (s *APIKeyService) invalidateRequestConfig(kind string, id int64) {
	message := fmt.Sprintf("%s%s:%d", RequestConfigInvalidationPrefix, kind, id)
	s.applyRequestConfigInvalidation(message)
	if s.cache == nil {
		return
	}
	// A canceled admin request must not leave committed changes invisible locally
	// or suppress notification to the other serving replicas.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.cache.PublishAuthCacheInvalidation(ctx, message); err != nil {
		slog.Warn("request configuration invalidation publish failed", "kind", kind, "id", id, "error", err)
	}
}

// InvalidateRequestCatalog is optional for existing administrative services.
func (s *APIKeyService) InvalidateRequestCatalog() { s.invalidateRequestConfig("catalog", 0) }

// InvalidateRequestGroup also works after deletion, when the group's keys can
// no longer be enumerated by the ordinary auth invalidator.
func (s *APIKeyService) InvalidateRequestGroup(groupID int64) {
	s.invalidateRequestConfig("group", groupID)
}

func (s *adminServiceImpl) invalidateRequestGroup(groupID int64) {
	if inv, ok := s.authCacheInvalidator.(interface{ InvalidateRequestGroup(int64) }); ok {
		inv.InvalidateRequestGroup(groupID)
	}
}

func (s *adminServiceImpl) invalidateRequestCatalog() {
	if inv, ok := s.authCacheInvalidator.(interface{ InvalidateRequestCatalog() }); ok {
		inv.InvalidateRequestCatalog()
	}
}

func (s *adminServiceImpl) invalidateCompositeRoutes(ctx context.Context, groupID int64) {
	// Keep directly constructed services correct as well as the production graph.
	if s.compositeResolver != nil {
		s.compositeResolver.routeCache.invalidate(func(key int64) bool { return key == groupID })
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
	}
}
