//go:build unit

package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthColdBurstQueuesAcrossSiteModes(t *testing.T) {
	for _, mode := range []string{site.Primary, site.Standalone, site.Secondary} {
		t.Run(mode, func(t *testing.T) {
			const requests, slots = 256, 64
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var active, peak atomic.Int64
			repo := &authRepoStub{getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &APIKey{ID: 1, UserID: 2, Key: key, Status: StatusActive, User: &User{ID: 2, Status: StatusActive, Balance: 10}}, nil
			}}
			cfg := &config.Config{Site: site.Config{Mode: mode}, APIKeyAuth: config.APIKeyAuthCacheConfig{LookupConcurrency: slots}}
			svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			done := make(chan error, requests)
			for i := range requests {
				go func() {
					key := fmt.Sprintf("cold-key-%d", i)
					value, err := svc.GetByKey(context.Background(), key)
					if err == nil && (value == nil || value.Key != key) {
						err = fmt.Errorf("incorrect identity returned for %s", key)
					}
					done <- err
				}()
			}
			require.Eventually(t, func() bool {
				metrics := svc.AuthLookupMetrics()
				return metrics.InFlight == slots && metrics.Waiting == requests-slots
			}, 3*time.Second, time.Millisecond, "a cold burst must queue without exceeding the DB query limit")
			close(release)
			for range requests {
				require.NoError(t, <-done)
			}
			require.Equal(t, int64(slots), peak.Load())
			require.Zero(t, svc.AuthLookupMetrics().Rejected)
			require.Zero(t, svc.AuthLookupMetrics().Waiting)
			require.Zero(t, svc.AuthLookupMetrics().InFlight)
		})
	}
}
