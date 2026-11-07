package service

import "context"

// Wire the existing authenticated invalidation subscriber into billing without
// adding a dependency cycle or changing repository interfaces. Called at startup.
func (s *APIKeyService) connectBillingRPMCache(billing *BillingCacheService) {
	if billing != nil && s.userGroupRateRepo != nil {
		billing.requestRPMOverride = s.requestRPMOverride
	}
}

func (s *APIKeyService) requestRPMOverride(ctx context.Context, userID, groupID int64) (*int, error) {
	load := func(ctx context.Context) (*int, int, error) {
		value, err := s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, userID, groupID)
		// nil means no override, while a pointer to zero explicitly exempts the
		// group RPM limit. Both are valid cached results; errors are not cached.
		return cloneGroupValuePointer(value), 32, err
	}
	var value *int
	var err error
	if s.requestConfigCacheReady() {
		value, err = s.requestRPMOverrides.get(ctx, userGroupPolicyKey{userID, groupID}, load)
	} else {
		value, _, err = load(ctx)
	}
	return cloneGroupValuePointer(value), err
}

func (s *BillingCacheService) loadRPMOverride(ctx context.Context, userID, groupID int64) (*int, error) {
	if s.requestRPMOverride != nil {
		return s.requestRPMOverride(ctx, userID, groupID)
	}
	return s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, userID, groupID)
}
