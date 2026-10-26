//go:build embed

package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/web"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type pelicanHTMLSettingsRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *pelicanHTMLSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *pelicanHTMLSettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func TestPelicanSettingsRefreshInvalidatesInjectedHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &pelicanHTMLSettingsRepo{values: map[string]string{}}
	settings := service.NewSettingService(repo, nil)
	frontend, err := web.NewFrontendServer(settings)
	require.NoError(t, err)
	settings.SetOnUpdateCallback(frontend.InvalidateCache)
	router := gin.New()
	router.Use(frontend.Middleware())
	getPage := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("If-None-Match", etag)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	for _, state := range []struct {
		name      string
		enabled   bool
		adminOnly bool
	}{
		{"enable administrator-only gallery", true, true},
		{"open gallery to users", true, false},
		{"restrict existing gallery", true, true},
		{"disable gallery", false, true},
	} {
		t.Run(state.name, func(t *testing.T) {
			before := getPage("")
			require.Equal(t, http.StatusOK, before.Code)
			etag := before.Header().Get("ETag")
			require.NotEmpty(t, etag)
			_, err := settings.UpdatePelicanShowcaseSettings(context.Background(), state.enabled, service.DefaultPelicanShowcaseConfig(), nil, &state.adminOnly)
			require.NoError(t, err)
			// Reload with the browser's old validator: a saved change must return
			// fresh injected settings rather than stale HTML or a 304 response.
			after := getPage(etag)
			require.Equal(t, http.StatusOK, after.Code)
			require.NotEqual(t, etag, after.Header().Get("ETag"))
			require.Contains(t, after.Body.String(), `"pelican_showcase_enabled":`+strconv.FormatBool(state.enabled))
			require.Contains(t, after.Body.String(), `"pelican_showcase_admin_only":`+strconv.FormatBool(state.adminOnly))
		})
	}
}
