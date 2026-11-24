package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUserErrorHistoryHasNoDetailRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterUserRoutes(router.Group("/api/v1"), &handler.Handlers{}, func(c *gin.Context) { c.AbortWithStatus(401) }, func(c *gin.Context) { c.Next() }, nil, nil)
	for path, status := range map[string]int{"/api/v1/usage/errors": 401, "/api/v1/usage/errors/742318": 404} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, status, w.Code, path)
	}
}
