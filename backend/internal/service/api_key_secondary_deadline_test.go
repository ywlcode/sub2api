//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSecondaryAuthDatabaseStallIsBounded(t *testing.T) {
	for _, path := range []string{"warm", "uncached", "install"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				key := secondaryCachedKey()
				blocked := false
				wait := func(ctx context.Context) error {
					deadline, ok := ctx.Deadline()
					require.True(t, ok, "a database read must not inherit an unbounded HTTP context")
					require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
					<-ctx.Done()
					return ctx.Err()
				}
				repo := &secondaryCachedAuthRepoStub{
					secondaryAuthRepoStub: secondaryAuthRepoStub{lookup: func(ctx context.Context, _ string) (*APIKey, error) {
						if blocked {
							return nil, wait(ctx)
						}
						return copySecondaryCachedKey(key), nil
					}},
					state: func(ctx context.Context, _ string) (*SecondaryAuthState, error) {
						return nil, wait(ctx)
					},
				}
				svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, secondaryAuthTestConfig())
				_, err := svc.GetByKey(context.Background(), "credential")
				require.NoError(t, err)
				blocked = true
				lookup := svc.GetByKey
				if path == "uncached" {
					lookup = svc.GetByKeyUncached
				} else if path == "install" {
					finish := svc.BeginSiteAuthInstall()
					defer finish()
				}
				started := time.Now()
				got, err := lookup(context.Background(), "credential")
				require.Nil(t, got, "a timeout must not return cached eligibility")
				require.ErrorIs(t, err, ErrAPIKeyAuthUnavailable)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, 5*time.Second, time.Since(started))
				require.Zero(t, svc.AuthLookupMetrics().InFlight)
				require.Zero(t, svc.AuthLookupMetrics().Waiting)

				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				started = time.Now()
				_, err = lookup(ctx, "credential")
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, 100*time.Millisecond, time.Since(started), "retain shorter admission budgets")
			})
		})
	}
}

func TestSecondaryReadDeadlineDoesNotChangePrimaryOrStandalone(t *testing.T) {
	for _, mode := range []string{site.Primary, site.Standalone} {
		t.Run(mode, func(t *testing.T) {
			cfg := secondaryAuthTestConfig()
			cfg.Site.Mode = mode
			repo := &authRepoStub{getByKeyForAuth: func(ctx context.Context, _ string) (*APIKey, error) {
				_, ok := ctx.Deadline()
				require.False(t, ok)
				return nil, ErrAPIKeyNotFound
			}}
			svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			_, err := svc.GetByKeyUncached(context.Background(), "credential")
			require.ErrorIs(t, err, ErrAPIKeyNotFound)
		})
	}
}
