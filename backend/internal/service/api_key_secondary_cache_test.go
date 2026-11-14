//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type secondaryCachedAuthRepoStub struct {
	secondaryAuthRepoStub
	state func(context.Context, string) (*SecondaryAuthState, error)
}

func (r *secondaryCachedAuthRepoStub) GetSecondaryAuthState(ctx context.Context, key string) (*SecondaryAuthState, error) {
	return r.state(ctx, key)
}

func secondaryCachedKey() *APIKey {
	group := &Group{ID: 3, Status: StatusActive, Platform: PlatformDeepseek, Hydrated: true,
		ModelRouting: map[string][]int64{"deepseek": {7, 8}}}
	return &APIKey{ID: 1, UserID: 2, GroupID: &group.ID, GroupIDs: []int64{3}, Group: group,
		Status: StatusActive, Quota: 10, QuotaUsed: 1, ModelMapping: map[string]string{"alias": "deepseek"},
		User: &User{ID: 2, Status: StatusActive, Balance: 9, AllowedGroups: []int64{3}, UserGroupDeniedModels: []string{"blocked"}}}
}

func secondaryLiveState(key *APIKey) *SecondaryAuthState {
	state := &SecondaryAuthState{KeyID: key.ID, UserID: key.UserID, GroupID: key.GroupID,
		KeyStatus: key.Status, Quota: key.Quota, QuotaUsed: key.QuotaUsed, ExpiresAt: key.ExpiresAt}
	if key.User != nil {
		state.UserExists, state.UserStatus, state.Balance = true, key.User.Status, key.User.Balance
	}
	if key.Group != nil {
		state.GroupExists, state.GroupStatus = true, key.Group.Status
	}
	return state
}

func copySecondaryCachedKey(key *APIKey) *APIKey {
	raw, _ := json.Marshal(key)
	var copy APIKey
	_ = json.Unmarshal(raw, &copy)
	return &copy
}

func TestSecondaryAuthCacheKeepsMoneyAndStatusFresh(t *testing.T) {
	ctx := context.Background()
	key := secondaryCachedKey()
	var full atomic.Int64
	var stateErr error
	repo := &secondaryCachedAuthRepoStub{
		secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(context.Context, string) (*APIKey, error) {
			full.Add(1)
			return copySecondaryCachedKey(key), nil
		}},
		state: func(context.Context, string) (*SecondaryAuthState, error) { return secondaryLiveState(key), stateErr },
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
	first, err := svc.GetByKey(ctx, "never-retain-this-credential")
	require.NoError(t, err)
	require.Equal(t, key.User.Balance, first.User.Balance)
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().Total, "a cold lookup must not duplicate its authoritative SQL read")
	require.Zero(t, svc.AuthLookupMetrics().SecondaryStateReads)
	// Downstream mutation must never corrupt another request's permissions.
	first.ModelMapping["alias"] = "corrupt"
	first.User.AllowedGroups[0] = 999
	first.Group.ModelRouting["deepseek"][0] = 999
	key.User.Balance, key.QuotaUsed, key.Status = 0, 10, StatusAPIKeyQuotaExhausted
	second, err := svc.GetByKey(ctx, "never-retain-this-credential")
	require.NoError(t, err)
	require.Equal(t, int64(1), full.Load(), "warm authorization must not reload the full projection")
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().SecondaryStateReads)
	require.Zero(t, second.User.Balance)
	require.Equal(t, float64(10), second.QuotaUsed)
	require.True(t, second.IsQuotaExhausted())
	require.Equal(t, StatusAPIKeyQuotaExhausted, second.Status)
	require.Equal(t, key.ModelMapping, second.ModelMapping)
	require.Equal(t, key.User.AllowedGroups, second.User.AllowedGroups)
	require.Equal(t, key.Group.ModelRouting, second.Group.ModelRouting)
	svc.secondaryAuthConfig.mu.Lock()
	for _, entry := range svc.secondaryAuthConfig.entries {
		require.NotContains(t, string(entry.value), "never-retain-this-credential")
		var cached APIKey
		require.NoError(t, json.Unmarshal(entry.value, &cached))
		require.Zero(t, cached.User.Balance)
		require.Zero(t, cached.QuotaUsed)
		require.Empty(t, cached.Status)
	}
	svc.secondaryAuthConfig.mu.Unlock()
	stateErr = errors.New("live database unavailable")
	_, err = svc.GetByKey(ctx, "never-retain-this-credential")
	require.ErrorIs(t, err, stateErr, "a cache hit must not bypass live eligibility failures")
	stateErr = ErrAPIKeyNotFound
	_, err = svc.GetByKey(ctx, "never-retain-this-credential")
	require.ErrorIs(t, err, ErrAPIKeyNotFound)
	stateErr = nil
	key.User, key.Group = nil, nil
	deleted, err := svc.GetByKey(ctx, "never-retain-this-credential")
	require.NoError(t, err)
	require.Nil(t, deleted.User)
	require.Nil(t, deleted.Group)
}

func TestSecondaryAuthCacheInstallInvalidatesPermissions(t *testing.T) {
	ctx := context.Background()
	key := secondaryCachedKey()
	repo := &secondaryCachedAuthRepoStub{
		secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(context.Context, string) (*APIKey, error) { return copySecondaryCachedKey(key), nil }},
		state:                 func(context.Context, string) (*SecondaryAuthState, error) { return secondaryLiveState(key), nil },
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
	_, err := svc.GetByKey(ctx, "credential")
	require.NoError(t, err)
	require.Equal(t, 1, svc.AuthLookupMetrics().SecondaryCacheEntries)
	finish := svc.BeginSiteAuthInstall()
	key.User.UserGroupDeniedModels = []string{"newly-blocked"}
	during, err := svc.GetByKey(ctx, "credential")
	require.NoError(t, err)
	require.Equal(t, key.User.UserGroupDeniedModels, during.User.UserGroupDeniedModels)
	require.Zero(t, svc.AuthLookupMetrics().SecondaryCacheEntries, "installing uses uncached auth")
	finish()
	after, err := svc.GetByKey(ctx, "credential")
	require.NoError(t, err)
	require.Equal(t, key.User.UserGroupDeniedModels, after.User.UserGroupDeniedModels)
	require.Equal(t, uint64(2), svc.AuthLookupMetrics().SecondaryConfigLoads)
	key.ModelMapping["alias"] = "uncached-authoritative"
	fresh, err := svc.GetByKeyUncached(ctx, "credential")
	require.NoError(t, err)
	require.Equal(t, "uncached-authoritative", fresh.ModelMapping["alias"])
}

func TestSecondaryAuthCacheFencesLoadsAndLiveReadsAcrossCommit(t *testing.T) {
	for _, blockLoad := range []bool{true, false} {
		name := "live_read"
		if blockLoad {
			name = "config_load"
		}
		t.Run(name, func(t *testing.T) {
			key := secondaryCachedKey()
			entered, release := make(chan struct{}), make(chan struct{})
			var armed, blocked atomic.Bool
			repo := &secondaryCachedAuthRepoStub{
				secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(context.Context, string) (*APIKey, error) {
					copy := copySecondaryCachedKey(key)
					if blockLoad && armed.Load() && blocked.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
					return copy, nil
				}},
				state: func(context.Context, string) (*SecondaryAuthState, error) {
					state := secondaryLiveState(key)
					if !blockLoad && armed.Load() && blocked.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
					return state, nil
				},
			}
			svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
			if !blockLoad {
				_, err := svc.GetByKey(context.Background(), "credential")
				require.NoError(t, err)
			}
			armed.Store(true)
			type result struct {
				key *APIKey
				err error
			}
			done := make(chan result, 1)
			go func() { key, err := svc.GetByKey(context.Background(), "credential"); done <- result{key, err} }()
			<-entered
			finish := svc.BeginSiteAuthInstall()
			key.User.UserGroupDeniedModels = []string{"revoked-during-query"}
			key.User.Balance = 0
			finish()
			close(release)
			got := <-done
			require.NoError(t, got.err)
			require.Equal(t, key.User.UserGroupDeniedModels, got.key.User.UserGroupDeniedModels)
			require.Zero(t, got.key.User.Balance)
			next, err := svc.GetByKey(context.Background(), "credential")
			require.NoError(t, err)
			require.Equal(t, key.User.UserGroupDeniedModels, next.User.UserGroupDeniedModels, "a slow old loader must not repopulate the cache")
		})
	}
}

func TestSecondaryAuthCacheCancelledLeaderDoesNotFailFollower(t *testing.T) {
	key := secondaryCachedKey()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	repo := &secondaryCachedAuthRepoStub{
		secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(ctx context.Context, _ string) (*APIKey, error) {
			if calls.Add(1) == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return copySecondaryCachedKey(key), nil
		}},
		state: func(context.Context, string) (*SecondaryAuthState, error) { return secondaryLiveState(key), nil },
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := svc.GetByKey(ctx, "credential"); first <- err }()
	<-entered
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	second := make(chan error, 1)
	go func() { _, err := svc.GetByKey(context.Background(), "credential"); second <- err }()
	close(release)
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("follower stuck")
	}
	require.Equal(t, int64(1), calls.Load())
}

func TestSecondaryAuthCacheDoesNotBypassBusyLiveState(t *testing.T) {
	key := secondaryCachedKey()
	repo := &secondaryCachedAuthRepoStub{
		secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(context.Context, string) (*APIKey, error) { return copySecondaryCachedKey(key), nil }},
		state:                 func(context.Context, string) (*SecondaryAuthState, error) { return secondaryLiveState(key), nil },
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
	_, err := svc.GetByKey(context.Background(), "credential")
	require.NoError(t, err)
	svc.authLookupSlots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = svc.GetByKey(ctx, "credential")
	<-svc.authLookupSlots
	require.ErrorIs(t, err, context.DeadlineExceeded, "a hot permission cache must not grant access without fresh money")
	require.Zero(t, svc.AuthLookupMetrics().Waiting)
	require.Zero(t, svc.AuthLookupMetrics().InFlight)
	key.User.Balance = 0
	latest, err := svc.GetByKey(context.Background(), "credential")
	require.NoError(t, err)
	require.Zero(t, latest.User.Balance)
	require.Equal(t, uint64(1), svc.AuthLookupMetrics().SecondaryConfigLoads)
}

func TestSecondaryAuthCacheReloadsReboundIdentity(t *testing.T) {
	for _, change := range []string{"key", "owner", "group", "unbound"} {
		t.Run(change, func(t *testing.T) {
			key := secondaryCachedKey()
			repo := &secondaryCachedAuthRepoStub{
				secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(context.Context, string) (*APIKey, error) { return copySecondaryCachedKey(key), nil }},
				state:                 func(context.Context, string) (*SecondaryAuthState, error) { return secondaryLiveState(key), nil },
			}
			svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
			_, err := svc.GetByKey(context.Background(), "credential")
			require.NoError(t, err)
			switch change {
			case "key":
				key.ID++
			case "owner":
				key.UserID++
				key.User.ID = key.UserID
			case "group":
				group := *key.Group
				group.ID++
				key.Group, key.GroupID, key.GroupIDs = &group, &group.ID, []int64{group.ID}
			case "unbound":
				key.Group, key.GroupID, key.GroupIDs = nil, nil, nil
			}
			key.ModelMapping["alias"] = "new-binding-only"
			latest, err := svc.GetByKey(context.Background(), "credential")
			require.NoError(t, err)
			require.Equal(t, key.ID, latest.ID)
			require.Equal(t, key.UserID, latest.UserID)
			require.Equal(t, key.GroupID, latest.GroupID)
			require.Equal(t, key.GroupIDs, latest.GroupIDs)
			require.Equal(t, key.ModelMapping, latest.ModelMapping)
			require.Zero(t, svc.AuthLookupMetrics().SecondaryCacheEntries, "a changed binding must discard its old config")
		})
	}
}
