//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type currentUserReadCounter struct {
	*userHandlerRepoStub
	userReads, avatarReads int
}

func (r *currentUserReadCounter) GetByID(ctx context.Context, id int64) (*service.User, error) {
	r.userReads++
	return r.userHandlerRepoStub.GetByID(ctx, id)
}

func (r *currentUserReadCounter) GetUserAvatar(ctx context.Context, id int64) (*service.UserAvatar, error) {
	r.avatarReads++
	return r.userHandlerRepoStub.GetUserAvatar(ctx, id)
}

func TestCurrentUserReusesJWTReadButChecksEachRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &currentUserReadCounter{userHandlerRepoStub: &userHandlerRepoStub{user: &service.User{
		ID: 31, Email: "me@example.com", Status: service.StatusActive,
		Role: service.RoleUser, TokenVersion: 1, Balance: 100,
		AvatarURL: "https://example.com/avatar.png", AvatarSource: "remote_url",
	}}}
	cfg := &config.Config{}
	cfg.JWT.Secret = "test-jwt-secret-32bytes-long!!!"
	cfg.JWT.AccessTokenExpireMinutes = 60
	auth := service.NewAuthService(nil, repo, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	users := service.NewUserService(repo, nil, nil, nil)
	h := &AuthHandler{userService: users}
	router := gin.New()
	router.GET("/api/v1/auth/me", gin.HandlerFunc(middleware.NewJWTAuthMiddleware(auth, users, nil, nil)), h.GetCurrentUser)
	token, err := auth.GenerateToken(context.Background(), repo.user)
	require.NoError(t, err)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	w := request()
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"avatar_url":"https://example.com/avatar.png"`)
	require.Equal(t, 1, repo.userReads)
	require.Equal(t, 1, repo.avatarReads)

	repo.user.Balance = 42
	w = request()
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"balance":42`)
	require.Equal(t, 2, repo.userReads, "the next HTTP request must load fresh authority")

	repo.user.TokenVersion++
	w = request()
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "TOKEN_REVOKED")
}
