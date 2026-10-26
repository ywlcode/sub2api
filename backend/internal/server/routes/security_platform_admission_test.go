package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type routePlatformBanRepo struct {
	service.ContentModerationRepository
}

func (routePlatformBanRepo) IsOpenAIBlocked(context.Context, int64) (bool, error) { return true, nil }

func TestGatewayRoutesPlatformBanCoversAliasesAndNonAuditedEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformComposite} {
		t.Run(platform, func(t *testing.T) {
			cfg := &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
			moderation := service.NewContentModerationService(nil, routePlatformBanRepo{}, nil, nil, nil, nil, nil, nil)
			h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(nil, nil, nil, nil, nil, nil, moderation, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
			router := gin.New()
			RegisterGatewayRoutes(router, h, func(c *gin.Context) {
				groupID := int64(3)
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 11, UserID: 7, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: platform, Status: service.StatusActive}, User: &service.User{ID: 7, Status: service.StatusActive}})
				c.Next()
			}, nil, nil, nil, nil, nil, cfg)
			for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses", "/v1/chat/completions", "/v1/embeddings", "/v1/messages/count_tokens"} {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-test","input":"benign"}`))
				req.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(w, req)
				require.Equal(t, http.StatusForbidden, w.Code, "%s: %s", path, w.Body.String())
				require.Contains(t, w.Body.String(), "openai_access_restricted", path)
			}
		})
	}
}
