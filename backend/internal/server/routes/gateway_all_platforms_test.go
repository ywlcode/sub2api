//go:build unit

package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/serverless"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type allPlatformsRouteRepository struct {
	service.ContentModerationRepository
	checked []int64
}

type gatewayUsageScopeRepo struct {
	service.UsageLogRepository
	summaryKey, modelKey, dailyKey, dailyUser int64
}

func (r *gatewayUsageScopeRepo) GetAPIKeyDashboardStats(_ context.Context, key int64) (*usagestats.UserDashboardStats, error) {
	r.summaryKey = key
	return &usagestats.UserDashboardStats{}, nil
}

func (r *gatewayUsageScopeRepo) GetModelStatsWithFilters(_ context.Context, _, _ time.Time, _, key, _, _ int64, _ *int16, _ *bool, _ *int8) ([]usagestats.ModelStat, error) {
	r.modelKey = key
	return nil, nil
}

func (r *gatewayUsageScopeRepo) GetUsageTrendWithFilters(_ context.Context, _, _ time.Time, _ string, user, key, _, _ int64, _ string, _ *int16, _ *bool, _ *int8) ([]usagestats.TrendDataPoint, error) {
	r.dailyUser, r.dailyKey = user, key
	return nil, nil
}

func TestGatewayRoutesAllPlatformsOwnUsageStillRequiresAuth(t *testing.T) {
	for _, path := range []string{"/v1/usage", "/antigravity/v1/usage"} {
		t.Run(path, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeStandard, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}}
			f := newUnionFixture()
			f.key.GroupIDs = []int64{f.key.Group.ID}
			if path == "/antigravity/v1/usage" {
				f.key.Group.Platform = service.PlatformAntigravity
			}
			f.key.Quota = 100
			keys := service.NewAPIKeyService(&unionKeys{key: f.key}, nil, f.groups, f.subs, f.rates, nil, cfg)
			access := &allPlatformsRouteRepository{}
			mod := service.NewContentModerationService(nil, access, nil, nil, nil, nil, nil, nil)
			scope := &gatewayUsageScopeRepo{}
			usage := service.NewUsageService(scope, nil, nil, nil)
			h := &handler.Handlers{
				Gateway:       handler.NewGatewayHandler(nil, nil, nil, nil, nil, nil, nil, usage, keys, nil, nil, mod, nil, cfg, nil),
				OpenAIGateway: handler.NewOpenAIGatewayHandler(nil, nil, nil, nil, nil, nil, mod, nil, cfg),
			}
			router := gin.New()
			RegisterGatewayRoutes(router, h, middleware.NewAPIKeyAuthMiddleware(keys, nil, cfg), keys, nil, nil, nil, nil, cfg)
			keys.SetGroupRouter(f.probe)

			unauth := httptest.NewRecorder()
			router.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusUnauthorized, unauth.Code)
			require.Contains(t, unauth.Body.String(), "API_KEY_REQUIRED")
			w := unionRequest(router, http.MethodGet, path+"?user_id=999&api_key_id=888")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, f.key.ID, scope.summaryKey)
			require.Equal(t, f.key.ID, scope.modelKey)
			require.Equal(t, f.key.ID, scope.dailyKey)
			require.Equal(t, f.key.UserID, scope.dailyUser)
			require.Empty(t, access.checked)
		})
	}
}

func (r *allPlatformsRouteRepository) ModerationPlatformAccess(_ context.Context, id int64) (bool, bool, error) {
	r.checked = append(r.checked, id)
	return true, false, nil
}

func TestGatewayRoutesAllPlatformsPreAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ method, path, platform string }{
		{"POST", "/v1/messages", service.PlatformAnthropic},
		{"POST", "/v1/responses", service.PlatformOpenAI},
		{"POST", "/chat/completions", service.PlatformKimi},
		{"POST", "/v1beta/models/gemini-test:generateContent", service.PlatformGemini},
		{"POST", "/antigravity/v1beta/models/gemini-test:generateContent", service.PlatformAntigravity},
		{"POST", "/antigravity/v1/messages/count_tokens", service.PlatformAntigravity},
		{"POST", "/v1/videos", service.PlatformMiniMax},
		{"POST", "/api/v3/contents/generations/tasks", service.PlatformComposite},
		{"POST", "/v1/images/batches", service.PlatformOpenAI},
		{"POST", "/images/generations/async", service.PlatformGrok},
		{"GET", "/realtime", service.PlatformGrok},
		{"GET", "/v1/tts", service.PlatformGrok},
		{"GET", "/v1/live/test-call", service.PlatformOpenAI},
		{"GET", "/backend-api/codex/responses", service.PlatformComposite},
		{"GET", "/responses", "pending-multi-group"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}}
			f := newUnionFixture()
			if tc.platform != "pending-multi-group" {
				f.key.GroupIDs = []int64{f.key.Group.ID}
				f.key.Group.Platform = tc.platform
			}
			keys := service.NewAPIKeyService(&unionKeys{key: f.key}, nil, f.groups, f.subs, f.rates, nil, cfg)
			repo := &allPlatformsRouteRepository{}
			mod := service.NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
			h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(nil, nil, nil, nil, nil, nil, mod, nil, cfg)}
			store := &serverlessAdmissionStore{}
			manager, err := serverless.New(serverless.Runtime{}, nil, store)
			require.NoError(t, err)
			t.Cleanup(manager.Stop)
			router := gin.New()
			RegisterGatewayRoutes(router, h, middleware.NewAPIKeyAuthMiddleware(keys, nil, cfg), keys, nil, nil, &service.SettingService{Serverless: manager}, nil, cfg)
			keys.SetGroupRouter(f.probe)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"shared-model"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer synthetic-union-key")
			if tc.method == "GET" {
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Connection", "upgrade")
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "all_platforms_access_restricted")
			require.Equal(t, []int64{f.key.UserID}, repo.checked)
			require.Zero(t, store.reads, "denied model requests must never reach pod routing")
		})
	}
}
