//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type expiryStartupRepo struct {
	ContentModerationRepository
}

func (expiryStartupRepo) ExpireModerationBans(context.Context, int) ([]int64, error) {
	return []int64{42}, nil
}

type expiryAuthInvalidator struct {
	APIKeyAuthCacheInvalidator
	recovered chan int64
}

func (a expiryAuthInvalidator) InvalidateAuthCacheByUserID(ctx context.Context, id int64) {
	select {
	case a.recovered <- id:
	case <-ctx.Done():
	}
}

func TestModerationBanExpiryRunsOnRestartWithoutAuditTraffic(t *testing.T) {
	recovered := make(chan int64, 1)
	s := &ContentModerationService{repo: expiryStartupRepo{}, authCacheInvalidator: expiryAuthInvalidator{recovered: recovered}}
	s.startBanExpiry()
	defer s.StopBanExpiry()
	select {
	case id := <-recovered:
		require.EqualValues(t, 42, id)
	case <-time.After(3 * time.Second):
		t.Fatal("expected recovery on startup without requests or an active audit configuration")
	}
}
