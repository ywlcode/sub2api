//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBillingErrorDetailsAfterWait(t *testing.T) {
	primary := int64(2)
	multi := &service.APIKey{GroupID: &primary, GroupIDs: []int64{2, 25}}
	for _, tc := range []struct {
		name string
		key  *service.APIKey
		err  error
		want int
	}{
		{"daily", multi, service.ErrDailyLimitExceeded, http.StatusBadGateway},
		{"weekly", multi, service.ErrWeeklyLimitExceeded, http.StatusBadGateway},
		{"monthly", multi, service.ErrMonthlyLimitExceeded, http.StatusBadGateway},
		{"wrapped quota", multi, fmt.Errorf("recheck: %w", service.ErrDailyLimitExceeded), http.StatusBadGateway},
		{"single group", &service.APIKey{GroupID: &primary, GroupIDs: []int64{2}}, service.ErrDailyLimitExceeded, http.StatusForbidden},
		{"legacy single group", &service.APIKey{GroupID: &primary}, service.ErrDailyLimitExceeded, http.StatusForbidden},
		{"missing key", nil, service.ErrDailyLimitExceeded, http.StatusForbidden},
		{"balance", multi, service.ErrInsufficientBalance, http.StatusForbidden},
		{"expired subscription", multi, service.ErrSubscriptionInvalid, http.StatusForbidden},
		{"key spending limit", multi, service.ErrAPIKeyRateLimit1dExceeded, http.StatusTooManyRequests},
		{"platform quota", multi, service.ErrUserPlatformDailyQuotaExhausted, http.StatusTooManyRequests},
		{"RPM", multi, service.ErrGroupRPMExceeded, http.StatusTooManyRequests},
		{"billing unavailable", multi, service.ErrBillingServiceUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, wantType, wantMessage, _ := billingErrorDetails(tc.err)
			status, errType, message, retryAfter := billingErrorDetailsAfterWait(tc.err, tc.key)
			require.Equal(t, tc.want, status)
			require.Equal(t, wantType, errType)
			require.Equal(t, wantMessage, message)
			if tc.want == http.StatusBadGateway {
				require.Zero(t, retryAfter)
				// The initial check retains its business error even for these keys.
				initialStatus, _, _, _ := billingErrorDetails(tc.err)
				require.Equal(t, http.StatusForbidden, initialStatus)
			}
		})
	}
}

type quotaRecheckCache struct {
	service.BillingCache
	mu   sync.Mutex
	data service.SubscriptionCacheData
}

func (s *quotaRecheckCache) GetSubscriptionCache(context.Context, int64, int64) (*service.SubscriptionCacheData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyData := s.data
	return &copyData, nil
}

// Reproduce quota exhaustion while waiting, including the sub-10s waits that
// previously returned an actual HTTP 403 before an SSE heartbeat was emitted.
func TestOpenAIMultiGroupQuotaExhaustedDuringWait(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, multi := range []bool{false, true} {
			for _, heartbeat := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/multi=%t/heartbeat=%t", endpoint, multi, heartbeat), func(t *testing.T) {
					upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response {
						t.Fatal("quota rejection must not call upstream")
						return nil
					}}
					limit := 200.0
					cache := &quotaRecheckCache{data: service.SubscriptionCacheData{
						Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour), DailyUsage: limit - 1,
					}}
					billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, &config.Config{}, nil)
					t.Cleanup(billing.Stop)
					repo := &initialAdmissionRepo{}
					h := newInitialAdmissionHandler(t, repo, upstream)
					h.billingCacheService = billing
					repo.slots.userSeq = []bool{false, true}
					repo.slots.waitAllowed = true
					repo.slots.waitIncrementHook = func() {
						cache.mu.Lock()
						cache.data.DailyUsage = limit
						cache.mu.Unlock()
					}
					pingInterval := time.Hour
					if heartbeat {
						pingInterval = time.Millisecond
					}
					h.concurrencyHelper = NewConcurrencyHelper(service.NewConcurrencyService(repo.slots), SSEPingFormatComment, pingInterval)
					c, rec := newExcelBPSFailoverTestContext(context.Background(), true)
					key, _ := middleware.GetAPIKeyFromContext(c)
					key.Group.SubscriptionType = service.SubscriptionTypeSubscription
					key.Group.DailyLimitUSD = &limit
					key.Group.AllowMessagesDispatch = true
					key.User.Concurrency = 5
					if multi {
						key.GroupIDs = []int64{*key.GroupID, 25}
					}
					subscription := &service.UserSubscription{UserID: key.User.ID, GroupID: *key.GroupID}
					c.Set(string(middleware.ContextKeySubscription), subscription)
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: 5})
					require.NoError(t, billing.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, subscription, service.PlatformOpenAI),
						"quota is available before the queue wait")
					c.Request.URL.Path = "/v1/" + endpoint
					switch endpoint {
					case "chat/completions", "messages":
						body := `{"model":"gpt-6-astra","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
						c.Request.Body = io.NopCloser(strings.NewReader(body))
						c.Request.ContentLength = int64(len(body))
					}
					switch endpoint {
					case "responses":
						h.Responses(c)
					case "chat/completions":
						h.ChatCompletions(c)
					case "messages":
						h.Messages(c)
					}
					wantStatus := http.StatusForbidden
					if multi {
						wantStatus = http.StatusBadGateway
					}
					if heartbeat {
						require.Equal(t, http.StatusOK, rec.Code, "committed SSE headers cannot change")
						if endpoint == "responses" {
							require.Contains(t, rec.Body.String(), "event: response.failed")
						} else {
							require.Contains(t, rec.Body.String(), "event: error")
						}
					} else {
						require.Equal(t, wantStatus, rec.Code, rec.Body.String())
					}
					require.Contains(t, rec.Body.String(), "daily usage limit exceeded")
					require.Contains(t, rec.Body.String(), "billing_error")
					require.Empty(t, upstream.calls())
					repo.slots.mu.Lock()
					acquired, released, dequeued := repo.slots.userAcquireCalls, repo.slots.userReleaseCalls, repo.slots.waitDecrementCalls
					keyReleased := repo.slots.apiKeyReleaseCalls
					repo.slots.mu.Unlock()
					require.Equal(t, 2, acquired)
					require.Equal(t, 1, released, "rejected request releases its user slot")
					require.Equal(t, 1, keyReleased, "rejected request releases its key tracking")
					require.Equal(t, 1, dequeued, "rejected request leaves the wait queue")
				})
			}
		}
	}
}
