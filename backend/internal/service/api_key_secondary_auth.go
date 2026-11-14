package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

const (
	secondaryAuthMaxWaiting  = 512
	secondaryAuthWaitTimeout = 5 * time.Second
	secondaryAuthReadTimeout = 5 * time.Second
)

// Optional repository extension: an authoritative local read of identity,
// permissions (including denied models), balance and Key quota in one snapshot.
// Primary/standalone keep their existing cache and repository path.
type secondaryAuthRepository interface {
	GetByKeyForSecondaryAuth(context.Context, string) (*APIKey, error)
}

// SecondaryAuthState is read on every admission, including cache hits and
// queued revalidation. Eligibility money and status never come from cached
// configuration; a cold load already includes them in its SQL snapshot.
type SecondaryAuthState struct {
	KeyID, UserID                      int64
	GroupID                            *int64
	KeyStatus, UserStatus, GroupStatus string
	UserExists, GroupExists            bool
	Balance, Quota, QuotaUsed          float64
	ExpiresAt                          *time.Time
}

type secondaryAuthStateRepository interface {
	GetSecondaryAuthState(context.Context, string) (*SecondaryAuthState, error)
}

func (s *APIKeyService) getSecondaryAPIKey(ctx context.Context, key string) (*APIKey, error) {
	reader, ok := s.apiKeyRepo.(secondaryAuthStateRepository)
	if !ok || s.localAuth != nil || s.cfg.Runtime.Role == config.RuntimeRoleGateway && s.cfg.Runtime.RegionalAuthFile != "" {
		return s.GetByKeyUncached(ctx, key)
	}
	if len(key) == 0 || len(key) > MaxAPIKeyCredentialBytes {
		return nil, ErrAPIKeyNotFound
	}
	epoch := s.secondaryAuthEpoch.Load()
	if s.secondaryAuthInstalling.Load() != 0 {
		return s.GetByKeyUncached(ctx, key)
	}
	digest := site.KeyDigest(s.cfg.Site.Token, key)
	var freshlyLoaded atomic.Pointer[APIKey]
	raw, err := s.secondaryAuthConfig.get(ctx, digest, func(loadCtx context.Context) ([]byte, int, error) {
		s.secondaryAuthConfigLoads.Add(1)
		current, err := s.GetByKeyUncached(loadCtx, key)
		if err != nil {
			return nil, 0, err
		}
		if current.User == nil {
			return nil, 0, ErrUserNotFound
		}
		// Serialize an owned immutable configuration. It contains neither the
		// credential nor balances/usage; decoding gives each request its own
		// maps, slices and nested policy objects, safe for downstream mutation.
		copy := *current
		owner := *current.User
		copy.User = &owner
		copy.Key, copy.Status, owner.Status = "", "", ""
		owner.Balance, owner.FrozenBalance = 0, 0
		copy.Quota, copy.QuotaUsed, copy.Usage5h, copy.Usage1d, copy.Usage7d = 0, 0, 0, 0, 0
		copy.ExpiresAt, copy.Window5hStart, copy.Window1dStart, copy.Window7dStart = nil, nil, nil, nil
		raw, err := json.Marshal(&copy)
		if err == nil {
			// Only this caller owns the complete snapshot. Do not keep its
			// money/status in the shared cache or give it to coalesced callers.
			freshlyLoaded.Store(current)
		}
		return raw, len(raw) + len(digest), err
	})
	if err != nil {
		return nil, err
	}
	if epoch != s.secondaryAuthEpoch.Load() || s.secondaryAuthInstalling.Load() != 0 {
		return s.GetByKeyUncached(ctx, key)
	}
	if fresh := freshlyLoaded.Load(); fresh != nil {
		// A cold lookup already read configuration and live state in one SQL
		// snapshot. A second query would only amplify cold-start DB pressure.
		return fresh, nil
	}
	var current APIKey
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, err
	}
	release, err := s.beginAuthLookup(ctx)
	if err != nil {
		return nil, err
	}
	s.secondaryAuthStateReads.Add(1)
	readCtx, cancel := context.WithTimeout(ctx, secondaryAuthReadTimeout)
	state, err := reader.GetSecondaryAuthState(readCtx, digest)
	err = secondaryAuthReadError(readCtx, err)
	cancel()
	release()
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, ErrAPIKeyNotFound
	}
	// The commit fence covers the whole cache + live-state read. An install
	// crossing this request must not mix old permissions with new money.
	if epoch != s.secondaryAuthEpoch.Load() || s.secondaryAuthInstalling.Load() != 0 {
		return s.GetByKeyUncached(ctx, key)
	}
	groupChanged := (current.GroupID == nil) != (state.GroupID == nil) ||
		current.GroupID != nil && state.GroupID != nil && *current.GroupID != *state.GroupID
	if current.ID != state.KeyID || current.UserID != state.UserID || groupChanged {
		s.secondaryAuthConfig.invalidate(func(k string) bool { return k == digest })
		return s.GetByKeyUncached(ctx, key)
	}
	current.Key, current.Status = key, state.KeyStatus
	current.Quota, current.QuotaUsed, current.ExpiresAt = state.Quota, state.QuotaUsed, state.ExpiresAt
	if !state.UserExists {
		current.User = nil
	} else if current.User != nil {
		current.User.Status, current.User.Balance = state.UserStatus, state.Balance
	}
	if !state.GroupExists {
		current.Group = nil
	} else if current.Group != nil {
		current.Group.Status = state.GroupStatus
	}
	s.compileAPIKeyIPRules(&current)
	return &current, nil
}

// A warm cache still needs live balances; replica installation also bypasses
// the cache. Neither path may wait indefinitely on a database lock. Keep the
// caller's shorter admission deadline and never serve stale money on timeout.
func secondaryAuthReadError(ctx context.Context, err error) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrAPIKeyAuthUnavailable.WithCause(err)
	}
	return err
}

// BeginSiteAuthInstall brackets the replica transaction's commit and policy
// installation without holding a lock over database I/O. During the bracket,
// requests use the authoritative single-statement path. Both ends fence slow
// loaders, including rollback and post-commit error paths.
func (s *APIKeyService) BeginSiteAuthInstall() func() {
	s.secondaryAuthInstalling.Add(1)
	s.invalidateSecondaryAuth()
	return func() {
		s.invalidateSecondaryAuth()
		s.secondaryAuthInstalling.Add(-1)
	}
}

func (s *APIKeyService) invalidateSecondaryAuth() {
	s.secondaryAuthEpoch.Add(1)
	s.secondaryAuthConfig.invalidate(nil)
}

func (s *APIKeyService) beginAuthLookup(ctx context.Context) (func(), error) {
	if s.authLookupSlots == nil {
		return func() {}, nil
	}
	s.authLookupTotal.Add(1)
	if err := s.acquireAuthLookupSlot(ctx); err != nil {
		if errors.Is(err, ErrAPIKeyAuthOverloaded) {
			s.authLookupRejected.Add(1)
		}
		return nil, err
	}
	s.authLookupInFlight.Add(1)
	return func() { s.authLookupInFlight.Add(-1); <-s.authLookupSlots }, nil
}

func (s *APIKeyService) acquireAuthLookupSlot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.authLookupSlots <- struct{}{}:
		return nil
	default:
	}
	// Cache misses on a primary still fail fast. A secondary reads the local
	// replica on every admission, so short bursts may wait without increasing
	// the number of simultaneous database queries. Bound both memory and time.
	if s.secondaryAuthWaiters == nil {
		return ErrAPIKeyAuthOverloaded
	}
	select {
	case s.secondaryAuthWaiters <- struct{}{}:
		defer func() { <-s.secondaryAuthWaiters }()
	default:
		return ErrAPIKeyAuthOverloaded
	}
	timer := time.NewTimer(secondaryAuthWaitTimeout)
	defer timer.Stop()
	select {
	case s.authLookupSlots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.authLookupSlots
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrAPIKeyAuthOverloaded
	}
}
