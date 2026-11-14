//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

type secondaryAuthRepoStub struct {
	authRepoStub
	lookup func(context.Context, string) (*APIKey, error)
}

func (r *secondaryAuthRepoStub) GetByKeyForSecondaryAuth(ctx context.Context, key string) (*APIKey, error) {
	return r.lookup(ctx, key)
}

func secondaryAuthTestConfig() *config.Config {
	return &config.Config{
		Site:       site.Config{Mode: site.Secondary, Token: "secondary-auth-test-token"},
		APIKeyAuth: config.APIKeyAuthCacheConfig{LookupConcurrency: 1},
	}
}

func TestSecondaryAuthFreshCombinedProjection(t *testing.T) {
	cfg := secondaryAuthTestConfig()
	state := &APIKey{ID: 1, UserID: 2, Status: StatusActive, Quota: 10,
		User: &User{ID: 2, Status: StatusActive, Balance: 10, UserGroupDeniedModels: []string{"denied"}}}
	var calls int
	var failure error
	repo := &secondaryAuthRepoStub{lookup: func(_ context.Context, key string) (*APIKey, error) {
		calls++
		require.Equal(t, site.KeyDigest(cfg.Site.Token, "credential"), key)
		if failure != nil {
			return nil, failure
		}
		copy := *state
		user := *state.User
		copy.User = &user
		return &copy, nil
	}}
	// Any separate permission read would fail: combined auth must supply it.
	svc := NewAPIKeyService(repo, nil, nil, nil, &uncachedKeyDeniedModelsRepo{err: errors.New("must not query twice")}, nil, cfg)
	first, err := svc.GetByKey(context.Background(), "credential")
	require.NoError(t, err)
	require.Equal(t, "credential", first.Key)
	require.Equal(t, []string{"denied"}, first.User.UserGroupDeniedModels)
	state.Status, state.User.Balance, state.QuotaUsed = StatusDisabled, 0, 10
	second, err := svc.GetByKey(context.Background(), "credential")
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, second.Status)
	require.Zero(t, second.User.Balance)
	require.True(t, second.IsQuotaExhausted())
	require.Equal(t, StatusActive, first.Status)
	failure = ErrAPIKeyNotFound
	_, err = svc.GetByKeyUncached(context.Background(), "credential")
	require.ErrorIs(t, err, ErrAPIKeyNotFound)
	require.Equal(t, 3, calls)
}

func TestSecondaryAuthWaitsWithoutIncreasingDBConcurrency(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var active, peak atomic.Int64
	repo := &authRepoStub{getByKeyForAuth: func(ctx context.Context, _ string) (*APIKey, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > peak.Load() {
			peak.Store(n)
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, ErrAPIKeyNotFound
	}}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
	done := make(chan error, 2)
	go func() { _, err := svc.GetByKey(context.Background(), "first"); done <- err }()
	<-entered
	go func() { _, err := svc.GetByKey(context.Background(), "second"); done <- err }()
	require.Eventually(t, func() bool { return svc.AuthLookupMetrics().Waiting == 1 }, time.Second, time.Millisecond)
	close(release)
	for range 2 {
		require.ErrorIs(t, <-done, ErrAPIKeyNotFound)
	}
	require.Equal(t, int64(1), peak.Load())
	require.Zero(t, svc.AuthLookupMetrics().Rejected)
	require.Zero(t, svc.AuthLookupMetrics().Waiting)
	require.Zero(t, svc.AuthLookupMetrics().InFlight)
}

func TestSecondaryAuthQueueBoundCancellationAndTimeout(t *testing.T) {
	svc := NewAPIKeyService(&authRepoStub{}, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
	svc.secondaryAuthWaiters = make(chan struct{}, 1)
	svc.authLookupSlots <- struct{}{} // Occupy the only database slot.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.GetByKey(ctx, "waiting"); done <- err }()
	require.Eventually(t, func() bool { return svc.AuthLookupMetrics().Waiting == 1 }, time.Second, time.Millisecond)
	_, err := svc.GetByKey(context.Background(), "overflow")
	require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Zero(t, svc.AuthLookupMetrics().Waiting)
	_, err = svc.GetByKey(context.Background(), "timeout")
	require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
	require.Zero(t, svc.AuthLookupMetrics().Waiting)
	require.Equal(t, uint64(2), svc.AuthLookupMetrics().Rejected)
	<-svc.authLookupSlots
	_, err = svc.GetByKey(ctx, "already-cancelled")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, svc.authLookupSlots)
}

func TestPrimaryAuthDoesNotUseSecondaryProjectionOrQueue(t *testing.T) {
	for _, mode := range []string{site.Primary, site.Standalone} {
		t.Run(string(mode), func(t *testing.T) {
			repo := &secondaryAuthRepoStub{authRepoStub: authRepoStub{getByKeyForAuth: func(_ context.Context, key string) (*APIKey, error) {
				require.Equal(t, "primary-credential", key)
				return nil, ErrAPIKeyNotFound
			}}}
			cfg := secondaryAuthTestConfig()
			cfg.Site.Mode = mode
			svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			require.Nil(t, svc.secondaryAuthWaiters)
			_, err := svc.GetByKey(context.Background(), "primary-credential")
			require.ErrorIs(t, err, ErrAPIKeyNotFound)
			svc.authLookupSlots <- struct{}{}
			_, err = svc.GetByKey(context.Background(), "busy")
			require.ErrorIs(t, err, ErrAPIKeyAuthOverloaded)
		})
	}
}
