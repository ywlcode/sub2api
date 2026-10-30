package service

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func clonePlatformConcurrencyLimits(limits map[string]int) map[string]int {
	out := make(map[string]int, len(limits))
	for platform, limit := range limits {
		out[platform] = limit
	}
	return out
}

func (s *adminServiceImpl) validatePlatformConcurrencyLimits(limits map[string]int) error {
	for platform, limit := range limits {
		if !domain.IsConcretePlatform(platform) || limit < 0 || int64(limit) > 2147483647 {
			return infraerrors.BadRequest("INVALID_PLATFORM_CONCURRENCY", "Platform concurrency must use a concrete platform and a non-negative integer")
		}
		if limit > 0 && s.cfg != nil && s.cfg.Runtime.RegionalConcurrencyFile != "" {
			return infraerrors.BadRequest("PLATFORM_CONCURRENCY_UNSUPPORTED", "Platform concurrency is not supported by regional concurrency mode")
		}
	}
	return nil
}

// UserPlatformConcurrencyCache admits both dimensions in one atomic operation.
// Limits of zero still track requests, so enabling a limit includes in-flight work.
type UserPlatformConcurrencyCache interface {
	AcquireUserPlatformSlot(context.Context, int64, string, int, int, string) (bool, error)
	ReleaseUserPlatformSlot(context.Context, int64, string, string) error
}

type userConcurrencySnapshot struct {
	userID int64
	total  int
	limits map[string]int
}
type userConcurrencyPolicy struct {
	platform   string
	current    atomic.Pointer[userConcurrencySnapshot]
	revalidate APIKeyQueueAuthRevalidator
}
type userConcurrencyPolicyKey struct{}

// WithUserConcurrencyPolicy captures routing fallback and immutable user limits.
// Auth revalidation publishes a fresh snapshot without mutating shared API keys.
func WithUserConcurrencyPolicy(ctx context.Context, key *APIKey, revalidate APIKeyQueueAuthRevalidator) context.Context {
	if key == nil || key.User == nil {
		return ctx
	}
	policy := &userConcurrencyPolicy{platform: PlatformFromAPIKey(key), revalidate: revalidate}
	policy.set(key.User)
	return context.WithValue(ctx, userConcurrencyPolicyKey{}, policy)
}
func (p *userConcurrencyPolicy) set(user *User) {
	p.current.Store(&userConcurrencySnapshot{userID: user.ID, total: user.Concurrency, limits: clonePlatformConcurrencyLimits(user.PlatformConcurrencyLimits)})
}
func userConcurrencyPolicyFromContext(ctx context.Context) *userConcurrencyPolicy {
	if ctx == nil {
		return nil
	}
	policy, _ := ctx.Value(userConcurrencyPolicyKey{}).(*userConcurrencyPolicy)
	return policy
}
func UpdateUserConcurrencyPolicy(ctx context.Context, user *User) {
	if policy := userConcurrencyPolicyFromContext(ctx); policy != nil && user != nil && policy.current.Load().userID == user.ID {
		policy.set(user)
	}
}

// RevalidateUserConcurrency refreshes through the existing auth cache only when
// retrying a user wait. WS already refreshes its policy at each turn admission.
func (s *ConcurrencyService) RevalidateUserConcurrency(ctx context.Context) error {
	policy := userConcurrencyPolicyFromContext(ctx)
	if policy == nil || policy.revalidate == nil {
		return nil
	}
	_, err := s.revalidateAPIKeyQueueAuth(ctx, policy.revalidate)
	return err
}

func userConcurrencyLimits(ctx context.Context, userID int64, fallback int) (string, int, int, error) {
	policy := userConcurrencyPolicyFromContext(ctx)
	if policy == nil {
		return "", fallback, 0, nil
	}
	snapshot := policy.current.Load()
	if snapshot.userID != userID {
		return "", 0, 0, fmt.Errorf("user concurrency identity mismatch")
	}
	platform := QuotaPlatform(ctx, &APIKey{Group: &Group{Platform: policy.platform}})
	if !domain.IsConcretePlatform(platform) {
		for _, limit := range snapshot.limits {
			if limit > 0 {
				return "", 0, 0, fmt.Errorf("cannot resolve platform for user concurrency")
			}
		}
		return "", snapshot.total, 0, nil
	}
	return platform, snapshot.total, snapshot.limits[platform], nil
}
