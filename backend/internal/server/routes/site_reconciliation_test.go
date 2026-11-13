package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSiteReconciliationRoutesRequireAdministrator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{SiteReconciliation: adminhandler.NewSiteReconciliationHandler(nil)}}
	auth := middleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
		} else {
			c.AbortWithStatus(http.StatusForbidden)
		}
	})
	RegisterAdminRoutes(r.Group("/api/v1"), h, auth, middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/sites/office/rejections"},
		{"GET", "/api/v1/admin/sites/office/rejections/1"},
		{"POST", "/api/v1/admin/sites/office/rejections/1/resolve"},
	} {
		for _, authorized := range []bool{false, true} {
			req := httptest.NewRequest(route.method, route.path, nil)
			want := http.StatusUnauthorized
			if authorized {
				req.Header.Set("Authorization", "Bearer ordinary-user")
				want = http.StatusForbidden
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, want, w.Code)
		}
	}
}
