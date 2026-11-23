//go:build unit

package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type monthlyRouteUsers struct {
	service.UserRepository
	user *service.User
}

func (r monthlyRouteUsers) GetByID(context.Context, int64) (*service.User, error) { return r.user, nil }
func (monthlyRouteUsers) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

func TestManualMonthlyUsageRoutesRequireAdmin(t *testing.T) {
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "monthly-usage-route-test", ExpireHour: 1}}
	auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, tc := range []struct {
		name, role string
		want       int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"ordinary user", service.RoleUser, http.StatusForbidden},
		{"observer", service.RoleObserver, http.StatusForbidden},
		{"administrator reaches ops handler", service.RoleAdmin, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := &service.User{ID: 7, Role: tc.role, Status: service.StatusActive}
			users := service.NewUserService(monthlyRouteUsers{user: user}, nil, nil, nil)
			router := gin.New()
			RegisterAdminRoutes(router.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{Ops: adminhandler.NewOpsHandler(nil)}},
				middleware.NewAdminAuthMiddleware(auth, users, nil, nil), middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }),
				middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
			var token string
			if tc.role != "" {
				var err error
				token, err = auth.GenerateToken(context.Background(), user)
				require.NoError(t, err)
			}
			for _, route := range []struct{ method, path string }{
				{http.MethodGet, "/config"},
				{http.MethodPost, "/monthly-usage/send"},
				{http.MethodGet, "/monthly-usage/jobs/febbc0a1-51d3-4644-8389-ec2db961f33e"},
			} {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(route.method, "/api/v1/admin/ops/email-notification"+route.path, nil)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				router.ServeHTTP(w, req)
				require.Equal(t, tc.want, w.Code, "%s %s: %s", route.method, route.path, w.Body.String())
			}
		})
	}
}
