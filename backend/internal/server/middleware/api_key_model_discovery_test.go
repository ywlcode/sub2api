//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModelDiscoveryDoesNotRequireSpendableFunds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []struct{ route, url string }{
		{"/v1/models", "/v1/models"},
		{"/v1/models", "/v1/models?client_version=0.167.0"},
		{"/models", "/models?client_version=0.167.0"},
		{"/backend-api/codex/models", "/backend-api/codex/models"},
		{"/v1/models/:model", "/v1/models/gpt-test"},
		{"/v1beta/models", "/v1beta/models"},
	} {
		for _, funds := range []string{"daily_limit", "weekly_limit", "monthly_limit", "zero_balance", "key_quota"} {
			t.Run(endpoint.url+"/"+funds, func(t *testing.T) {
				limit := 1.0
				group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformOpenAI}
				key := &service.APIKey{ID: 100, UserID: 7, Key: "test-key", Status: service.StatusActive, GroupID: &group.ID, Group: group,
					User: &service.User{ID: 7, Status: service.StatusActive, Role: service.RoleUser, Balance: 10, Concurrency: 5}}
				now := time.Now()
				sub := &service.UserSubscription{ID: 55, UserID: 7, GroupID: 42, Status: service.SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
				switch funds {
				case "daily_limit":
					group.SubscriptionType, group.DailyLimitUSD, sub.DailyUsageUSD = service.SubscriptionTypeSubscription, &limit, 2
				case "weekly_limit":
					group.SubscriptionType, group.WeeklyLimitUSD, sub.WeeklyUsageUSD = service.SubscriptionTypeSubscription, &limit, 2
				case "monthly_limit":
					group.SubscriptionType, group.MonthlyLimitUSD, sub.MonthlyUsageUSD = service.SubscriptionTypeSubscription, &limit, 2
				case "zero_balance":
					key.User.Balance = 0
				case "key_quota":
					key.Status = service.StatusAPIKeyQuotaExhausted
				}
				cfg := &config.Config{RunMode: config.RunModeStandard}
				keys := service.NewAPIKeyService(&stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}, nil, nil, nil, nil, nil, cfg)
				subs := service.NewSubscriptionService(nil, &stubUserSubscriptionRepo{getActive: func(context.Context, int64, int64) (*service.UserSubscription, error) { return sub, nil }}, nil, nil, cfg)
				t.Cleanup(subs.Stop)
				router := gin.New()
				if endpoint.route == "/v1beta/models" {
					router.Use(APIKeyAuthWithSubscriptionGoogle(keys, subs, cfg))
				} else {
					router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, subs, cfg)))
				}
				router.GET(endpoint.route, func(c *gin.Context) { c.Status(http.StatusOK) })
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, endpoint.url, nil)
				req.Header.Set("Authorization", "Bearer "+key.Key)
				router.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, "%s", w.Body.String())
			})
		}
	}
}

func TestModelDiscoveryRetainsAuthentication(t *testing.T) {
	for _, google := range []bool{false, true} {
		for _, scenario := range []string{"missing", "disabled_key", "disabled_user", "expired_key", "disabled_group", "ip_denied"} {
			t.Run(scenario+"/google="+map[bool]string{false: "false", true: "true"}[google], func(t *testing.T) {
				key := queueAuthTestKey(service.StatusActive, 1)
				status := http.StatusUnauthorized
				switch scenario {
				case "disabled_key":
					key.Status = service.StatusAPIKeyDisabled
				case "disabled_user":
					key.User.Status = service.StatusDisabled
				case "expired_key":
					key.Status, status = service.StatusAPIKeyExpired, http.StatusForbidden
				case "disabled_group":
					key.Group.Status, status = service.StatusDisabled, http.StatusForbidden
				case "ip_denied":
					key.IPWhitelist, status = []string{"10.255.255.1"}, http.StatusForbidden
				}
				cfg := &config.Config{RunMode: config.RunModeStandard}
				keys := service.NewAPIKeyService(&stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil }}, nil, nil, nil, nil, nil, cfg)
				r := gin.New()
				path := "/v1/models"
				if google {
					path = "/v1beta/models"
					r.Use(APIKeyAuthWithSubscriptionGoogle(keys, nil, cfg))
				} else {
					r.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)))
				}
				r.GET(path, func(c *gin.Context) { t.Error("unauthorized model discovery reached handler") })
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if scenario != "missing" {
					req.Header.Set("Authorization", "Bearer "+key.Key)
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				require.Equal(t, status, w.Code, "%s", w.Body.String())
			})
		}
	}
}

func TestModelDiscoveryQueueRevalidationSkipsOnlyQuota(t *testing.T) {
	key := queueAuthTestKey(service.StatusActive, 1)
	latest := queueAuthTestKey(service.StatusAPIKeyQuotaExhausted, 1)
	lookup := func(context.Context, string) (*service.APIKey, error) { return latest, nil }
	read := newAPIKeyAuthRevalidator(lookup, key.Key, "", key, false)
	generate := newAPIKeyAuthRevalidator(lookup, key.Key, "", key, true)
	_, err := read(t.Context())
	require.NoError(t, err)
	_, err = generate(t.Context())
	require.Error(t, err)
	latest.Status = service.StatusAPIKeyDisabled
	_, err = read(t.Context())
	require.Equal(t, http.StatusUnauthorized, infraerrors.Code(err))
	latest.Status = service.StatusAPIKeyExpired
	_, err = read(t.Context())
	require.Error(t, err)
}

func TestModelDiscoveryAllowlistDoesNotMatchGeneration(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, path := range []string{"/v1/responses", "/responses", "/v1/models", "/v1beta/models/*modelAction", "/v1/messages"} {
			r := gin.New()
			r.Handle(method, path, func(c *gin.Context) {
				want := method == http.MethodGet && path == "/v1/models"
				require.Equal(t, want, isModelDiscoveryRequest(c), "%s %s", method, path)
			})
			url := path
			if path == "/v1beta/models/*modelAction" {
				url = "/v1beta/models/gemini-test:generateContent"
			}
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, url+"?client_version=test", nil))
		}
	}
}
