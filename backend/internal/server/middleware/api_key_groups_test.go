//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type modelListGroupRepo struct {
	service.GroupRepository
	groups map[int64]*service.Group
}

func (r modelListGroupRepo) GetByIDLite(_ context.Context, id int64) (*service.Group, error) {
	if group := r.groups[id]; group != nil {
		return group, nil
	}
	return nil, service.ErrGroupNotFound
}

type modelListGroupProbe struct {
	calls int
}

func (p *modelListGroupProbe) ProbeAPIKeyGroup(context.Context, *service.Group, string, string) (service.APIKeyGroupAvailability, error) {
	p.calls++
	return service.APIKeyGroupAvailability{}, nil
}

func TestAPIKeyModelListGroupSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	paths := []string{"/v1/models", "/models", "/backend-api/codex/models"}
	for _, path := range paths {
		for _, scenario := range []string{
			"active subscription",
			"fallback after expired subscription",
			"fallback after inactive group",
			"all groups unauthorized",
			"invalid credential",
			"subscription quota remains enforced",
		} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				now := time.Now()
				limit := 200.0
				primary := &service.Group{
					ID: 1, Platform: service.PlatformComposite, Status: service.StatusActive, Hydrated: true,
					SubscriptionType: service.SubscriptionTypeSubscription, DailyLimitUSD: &limit,
				}
				fallback := &service.Group{
					ID: 2, Platform: service.PlatformComposite, Status: service.StatusActive, Hydrated: true,
					SubscriptionType: service.SubscriptionTypeStandard,
				}
				key := &service.APIKey{
					ID: 10, UserID: 7, Key: "model-list-test-key", Status: service.StatusActive,
					GroupID: &primary.ID, GroupIDs: []int64{primary.ID, fallback.ID}, Group: primary,
					User: &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive},
				}
				sub := &service.UserSubscription{
					ID: 20, UserID: key.UserID, GroupID: primary.ID, Status: service.SubscriptionStatusActive,
					ExpiresAt: now.Add(time.Hour), DailyUsageUSD: 100,
					DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now,
				}
				wantStatus, wantGroup := http.StatusOK, primary.ID
				credential := key.Key
				switch scenario {
				case "fallback after expired subscription":
					sub.ExpiresAt = now.Add(-time.Hour)
					key.User.Balance = 10
					wantGroup = fallback.ID
				case "fallback after inactive group":
					primary.Status = "inactive"
					key.User.Balance = 10
					wantGroup = fallback.ID
				case "all groups unauthorized":
					sub.ExpiresAt = now.Add(-time.Hour)
					fallback.IsExclusive = true
					wantStatus = http.StatusForbidden
				case "invalid credential":
					credential = "invalid-key"
					wantStatus = http.StatusUnauthorized
				case "subscription quota remains enforced":
					sub.DailyUsageUSD = limit + 1
					wantStatus = http.StatusTooManyRequests
				}

				cfg := &config.Config{RunMode: config.RunModeStandard}
				keyRepo := &stubApiKeyRepo{getByKey: func(_ context.Context, credential string) (*service.APIKey, error) {
					if credential != key.Key {
						return nil, service.ErrAPIKeyNotFound
					}
					return key, nil
				}}
				subRepo := &stubUserSubscriptionRepo{getActive: func(context.Context, int64, int64) (*service.UserSubscription, error) {
					return sub, nil
				}}
				keys := service.NewAPIKeyService(keyRepo, nil, modelListGroupRepo{groups: map[int64]*service.Group{
					primary.ID: primary, fallback.ID: fallback,
				}}, subRepo, nil, nil, cfg)
				probe := &modelListGroupProbe{}
				keys.SetGroupRouter(probe)
				subscriptions := service.NewSubscriptionService(nil, subRepo, nil, nil, cfg)
				t.Cleanup(subscriptions.Stop)
				router := gin.New()
				router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, subscriptions, cfg)))
				router.GET(path, func(c *gin.Context) {
					selected, ok := GetAPIKeyFromContext(c)
					require.True(t, ok)
					require.Equal(t, wantGroup, *selected.GroupID)
					c.Status(http.StatusOK)
				})

				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path+"?client_version=0.164.0", nil)
				req.Header.Set("Authorization", "Bearer "+credential)
				router.ServeHTTP(w, req)

				require.Equal(t, wantStatus, w.Code, "%s", w.Body.String())
				require.Zero(t, probe.calls, "model lists must not depend on live inference accounts")
				require.Equal(t, primary.ID, *key.GroupID, "selection must not mutate the shared auth key")
			})
		}
	}
}

func TestAPIKeyModelListExceptionDoesNotBypassOtherRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/responses", `{"model":"gpt-test"}`},
		{http.MethodGet, "/v1/responses", ""},
		{http.MethodGet, "/responses", ""},
		{http.MethodGet, "/backend-api/codex/responses", ""},
		{http.MethodGet, "/v1/models/:model", ""},
		{http.MethodGet, "/v1beta/models", ""},
		{http.MethodPost, "/v1/models", `{"model":"gpt-test"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			groups := map[int64]*service.Group{
				1: {ID: 1, Status: service.StatusActive, Platform: service.PlatformComposite, Hydrated: true},
				2: {ID: 2, Status: service.StatusActive, Platform: service.PlatformComposite, Hydrated: true},
			}
			key := &service.APIKey{
				ID: 10, UserID: 7, Key: "test-key", Status: service.StatusActive,
				GroupID: &groups[1].ID, GroupIDs: []int64{1, 2}, Group: groups[1],
				User: &service.User{ID: 7, Status: service.StatusActive, Balance: 10},
			}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			keys := service.NewAPIKeyService(&stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
				return key, nil
			}}, nil, modelListGroupRepo{groups: groups}, nil, nil, nil, cfg)
			probe := &modelListGroupProbe{}
			keys.SetGroupRouter(probe)
			router := gin.New()
			router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)))
			router.Handle(tc.method, tc.path, func(c *gin.Context) { c.Status(http.StatusOK) })
			req := httptest.NewRequest(tc.method, strings.ReplaceAll(tc.path, ":model", "gpt-test"), strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusServiceUnavailable, w.Code, "%s", w.Body.String())
			require.Contains(t, w.Body.String(), "NO_AVAILABLE_API_KEY_GROUP")
			require.Equal(t, 2, probe.calls, "non-catalog routes must still probe both groups")
		})
	}
}
