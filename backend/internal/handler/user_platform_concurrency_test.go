package handler

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type helperPlatformCache struct {
	helperConcurrencyCacheStub
	platform      string
	limit         int
	releases      int
	allow         bool
	allowPlatform string
}

func (c *helperPlatformCache) AcquireUserPlatformSlot(_ context.Context, _ int64, platform string, _ int, limit int, _ string) (bool, error) {
	c.platform, c.limit = platform, limit
	return c.allow && (c.allowPlatform == "" || c.allowPlatform == platform), nil
}

func TestUserPlatformConcurrencyFallbackReleasesBeforeReadmission(t *testing.T) {
	cache := &helperPlatformCache{allow: true, allowPlatform: service.PlatformAntigravity}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	key := &service.APIKey{ID: 77, User: &service.User{ID: 202, Concurrency: 3, PlatformConcurrencyLimits: map[string]int{service.PlatformAntigravity: 1, service.PlatformAnthropic: 2}}, Group: &service.Group{Platform: service.PlatformAntigravity}}
	c.Request = c.Request.WithContext(service.WithUserConcurrencyPolicy(c.Request.Context(), key, nil))
	started := false
	release, err := helper.AcquireUserSlotWithWait(c, 202, 3, 77, 0, false, &started)
	require.NoError(t, err)
	next, err := helper.reacquireUserSlotForPlatform(c, key, service.PlatformAnthropic, release, false, &started)
	require.Error(t, err)
	require.Nil(t, next) // no queue capacity: target is full
	require.Equal(t, service.PlatformAnthropic, cache.platform)
	require.Equal(t, 2, cache.limit)
	require.Equal(t, 1, cache.releases)
	require.Equal(t, 2, cache.apiKeyReleaseCalls) // old key plus refused new admission
	release()
	require.Equal(t, 1, cache.releases)
}
func (c *helperPlatformCache) ReleaseUserPlatformSlot(context.Context, int64, string, string) error {
	c.releases++
	return nil
}
func platformContext(ctx context.Context, revalidate service.APIKeyQueueAuthRevalidator) context.Context {
	return service.WithUserConcurrencyPolicy(ctx, &service.APIKey{User: &service.User{ID: 202, Concurrency: 3, PlatformConcurrencyLimits: map[string]int{service.PlatformOpenAI: 1}}, Group: &service.Group{Platform: service.PlatformOpenAI}}, revalidate)
}

func TestUserPlatformConcurrencyHTTPAndWSCleanup(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "ws"}[ws], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cache := &helperPlatformCache{allow: true}
				helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
				ctx, cancel := context.WithCancel(platformContext(context.Background(), nil))
				var release func()
				var acquired bool
				var err error
				if ws {
					release, acquired, err = helper.TryAcquireWSUserSlotForAPIKey(ctx, 202, 3, 77, 0)
				} else {
					release, acquired, err = helper.TryAcquireUserSlotForAPIKey(ctx, 202, 3, 77, 0)
				}
				require.NoError(t, err)
				require.True(t, acquired)
				require.Equal(t, service.PlatformOpenAI, cache.platform)
				cancel()
				synctest.Wait()
				if ws {
					require.Zero(t, cache.releases)
				} else {
					require.Equal(t, 1, cache.releases)
				}
				release()
				release()
				require.Equal(t, 1, cache.releases)
			})
		})
	}
}

func TestUserPlatformConcurrencyHTTPWaitRefresh(t *testing.T) {
	cache := &helperPlatformCache{}
	cache.waitAllowed = true
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
	refreshed := 0
	c.Request = c.Request.WithContext(platformContext(c.Request.Context(), func(ctx context.Context) (int, error) {
		refreshed++
		cache.allow = true
		service.UpdateUserConcurrencyPolicy(ctx, &service.User{ID: 202, Concurrency: 3, PlatformConcurrencyLimits: map[string]int{service.PlatformOpenAI: 2}})
		return 0, nil
	}))
	started := false
	release, err := helper.acquireUserSlotWithWaitTimeout(c, 202, 3, 0, 0, time.Second, false, &started)
	require.NoError(t, err)
	release()
	require.Equal(t, 1, refreshed)
	require.Equal(t, 2, cache.limit)
	require.Equal(t, 1, cache.waitDecrementCalls)
	require.Equal(t, 1, cache.releases)
}

func TestUserPlatformConcurrencyWaitTimeoutAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancelRequest], func(t *testing.T) {
			cache := &helperPlatformCache{}
			cache.waitAllowed = true
			helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
			c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
			ctx, cancel := context.WithCancel(platformContext(c.Request.Context(), nil))
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			if cancelRequest {
				timer := time.AfterFunc(5*time.Millisecond, cancel)
				defer timer.Stop()
			}
			started := false
			release, err := helper.acquireUserSlotWithWaitTimeout(c, 202, 3, 77, 0, 20*time.Millisecond, false, &started)
			require.Error(t, err)
			require.Nil(t, release)
			if cancelRequest {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Equal(t, 1, cache.waitDecrementCalls)
			require.Zero(t, cache.releases)
			require.Equal(t, 1, cache.apiKeyReleaseCalls)
		})
	}
}
