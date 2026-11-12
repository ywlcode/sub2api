package service

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
)

type userGroupPolicyKey struct{ userID, groupID int64 }
type userGroupPolicy struct {
	RPM    *int
	Denied []string
}

// Optional batched read; older repositories and fixtures retain their behavior.
type userGroupPolicyReader interface {
	GetRequestPolicy(context.Context, int64, int64) (*int, []string, error)
}

func (s *APIKeyService) requestConfigCacheReady() bool {
	if s != nil && s.cfg.IsSecondary() {
		return false
	}
	return s.localAuth == nil && s.cache != nil && s.authInvalidationConnected.Load()
}

func (s *APIKeyService) requestGroup(ctx context.Context, id int64) (*Group, error) {
	if !s.requestConfigCacheReady() {
		return s.groupRepo.GetByIDLite(ctx, id)
	}
	raw, err := s.requestGroups.get(ctx, id, func(ctx context.Context) ([]byte, int, error) {
		group, err := s.groupRepo.GetByIDLite(ctx, id)
		if err != nil {
			return nil, 0, err
		}
		raw, err := json.Marshal(group)
		return raw, len(raw), err
	})
	if err != nil {
		return nil, err
	}
	// Group contains many nested mutable settings. Decode this small snapshot to
	// give each request complete ownership, preserving all current/future fields.
	var group Group
	if err := json.Unmarshal(raw, &group); err != nil {
		return nil, err
	}
	return &group, nil
}

func (s *APIKeyService) requestGroupPolicy(ctx context.Context, userID, groupID int64) (userGroupPolicy, error) {
	load := func(ctx context.Context) (userGroupPolicy, int, error) {
		var policy userGroupPolicy
		var err error
		if reader, ok := s.userGroupRateRepo.(userGroupPolicyReader); ok {
			policy.RPM, policy.Denied, err = reader.GetRequestPolicy(ctx, userID, groupID)
		} else {
			policy.RPM, err = s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, userID, groupID)
			if err == nil {
				policy.Denied, err = s.userGroupRateRepo.GetDeniedModelsByUserAndGroup(ctx, userID, groupID)
			}
		}
		cost := 64
		for _, model := range policy.Denied {
			cost += 16 + len(model)
		}
		policy.RPM = cloneGroupValuePointer(policy.RPM)
		policy.Denied = slices.Clone(policy.Denied)
		return policy, cost, err
	}
	var policy userGroupPolicy
	var err error
	if s.requestConfigCacheReady() {
		policy, err = s.requestPolicies.get(ctx, userGroupPolicyKey{userID, groupID}, load)
	} else {
		policy, _, err = load(ctx)
	}
	policy.RPM = cloneGroupValuePointer(policy.RPM)
	policy.Denied = slices.Clone(policy.Denied)
	return policy, err
}

func (s *APIKeyService) apiKeyWithCachedGroup(ctx context.Context, key *APIKey, group *Group) (*APIKey, error) {
	copyKey := *key
	copyKey.ModelMapping = maps.Clone(key.ModelMapping)
	copyUser := *key.User
	copyKey.User = &copyUser
	id := group.ID
	copyKey.GroupID, copyKey.Group = &id, group
	copyUser.UserGroupRPMOverride, copyUser.UserGroupDeniedModels = nil, nil
	if s.userGroupRateRepo != nil {
		policy, err := s.requestGroupPolicy(ctx, key.UserID, id)
		if err != nil {
			return nil, err
		}
		copyUser.UserGroupRPMOverride, copyUser.UserGroupDeniedModels = policy.RPM, policy.Denied
	}
	return &copyKey, nil
}
