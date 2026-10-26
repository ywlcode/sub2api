//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type showcaseSettingRepo struct {
	toggleSettingRepo
}

func TestPelicanShowcaseHandler_AdminOnlyProtectsListAndHTML(t *testing.T) {
	for _, role := range []string{"", service.RoleUser, service.RoleObserver, service.RoleAdmin} {
		for _, item := range []bool{false, true} {
			t.Run(role+strconv.FormatBool(item), func(t *testing.T) {
				repo := &showcaseHandlerRepo{groups: []*service.PelicanShowcaseGroup{{ID: 3, Name: "admin-gallery"}},
					item: &service.PelicanShowcaseItem{ID: 7, GroupID: 3, ResponseText: "<svg>admin-only-artwork</svg>"}}
				h := newShowcaseHandler(map[string]string{service.SettingKeyPelicanShowcaseEnabled: "true", service.SettingKeyPelicanShowcaseAdminOnly: "true"}, repo)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set(string(middleware.ContextKeyUserRole), role)
				c.Params = gin.Params{{Key: "id", Value: "7"}}
				c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pelican-showcase", nil)
				if item {
					h.GetItem(c)
				} else {
					h.List(c)
				}
				if role == service.RoleAdmin {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				} else {
					require.Equal(t, http.StatusForbidden, w.Code)
					require.Contains(t, w.Body.String(), "PELICAN_SHOWCASE_ADMIN_ONLY")
					require.NotContains(t, w.Body.String(), "admin-gallery")
					require.NotContains(t, w.Body.String(), "admin-only-artwork")
				}
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			})
		}
	}
}

func (r *showcaseSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *showcaseSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

type showcaseHandlerRepo struct {
	service.PelicanShowcaseRepository
	groups  []*service.PelicanShowcaseGroup
	items   []*service.PelicanShowcaseItem
	item    *service.PelicanShowcaseItem
	deleted bool
}

func (r *showcaseHandlerRepo) ListGroups(context.Context) ([]*service.PelicanShowcaseGroup, error) {
	return r.groups, nil
}
func (r *showcaseHandlerRepo) ListItems(context.Context, []int64, int, time.Time) ([]*service.PelicanShowcaseItem, error) {
	return r.items, nil
}
func (r *showcaseHandlerRepo) GetItem(context.Context, int64, int, time.Time) (*service.PelicanShowcaseItem, error) {
	return r.item, nil
}
func (r *showcaseHandlerRepo) Delete(context.Context, int64) (bool, error) { return r.deleted, nil }

func newShowcaseHandler(values map[string]string, repo *showcaseHandlerRepo) *PelicanShowcaseHandler {
	settings := service.NewSettingService(&showcaseSettingRepo{toggleSettingRepo{values: values}}, &config.Config{})
	return NewPelicanShowcaseHandler(service.NewPelicanShowcaseService(repo, settings))
}

func serveShowcase(method, path string, handle gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Handle(method, path, handle)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path[:len(path)-len(":id")]+"7", nil))
	return w
}

func TestPelicanShowcaseHandler_DisabledGalleryIsEmpty(t *testing.T) {
	repo := &showcaseHandlerRepo{groups: []*service.PelicanShowcaseGroup{{ID: 1, Name: "hidden"}}}
	h := newShowcaseHandler(map[string]string{service.SettingKeyPelicanShowcaseConfig: `{"max_items":20}`}, repo)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pelican-showcase", nil)
	h.List(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":false,"api_enabled":false,"max_items":20,"retention_days":0,"groups":[]}}`, w.Body.String())

	require.Equal(t, http.StatusNotFound, serveShowcase(http.MethodGet, "/items/:id", h.GetItem).Code)
}

func TestPelicanShowcaseHandler_ListExposesNoAccountIdentity(t *testing.T) {
	generated := time.Date(2026, 9, 24, 8, 30, 0, 0, time.UTC)
	repo := &showcaseHandlerRepo{
		groups: []*service.PelicanShowcaseGroup{{ID: 3, Name: "VIP", Platform: "openai"}},
		items:  []*service.PelicanShowcaseItem{{ID: 7, GroupID: 3, ModelID: "gpt-6-astra", ReasoningEffort: "high", LatencyMs: 4200, GeneratedAt: generated}},
	}
	h := newShowcaseHandler(map[string]string{
		service.SettingKeyPelicanShowcaseEnabled: "true",
		service.SettingKeyPelicanShowcaseConfig:  `{"max_items":10,"auto_cleanup":true,"retention_days":5}`,
	}, repo)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pelican-showcase", nil)
	h.List(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":true,"api_enabled":true,"max_items":10,"retention_days":5,"groups":[
		{"id":3,"name":"VIP","platform":"openai","items":[
			{"id":7,"group_id":3,"model_id":"gpt-6-astra","reasoning_effort":"high","latency_ms":4200,"generated_at":"2026-09-24T08:30:00Z"}
		]}]}}`, w.Body.String())
}

func TestPelicanShowcaseHandler_ItemAndAdminDelete(t *testing.T) {
	repo := &showcaseHandlerRepo{}
	h := newShowcaseHandler(map[string]string{
		service.SettingKeyPelicanShowcaseEnabled: "true",
		service.SettingKeyPelicanShowcaseConfig:  `{}`,
	}, repo)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "abc"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pelican-showcase/items/abc", nil)
	h.GetItem(c)
	require.Equal(t, http.StatusBadRequest, w.Code)

	require.Equal(t, http.StatusNotFound, serveShowcase(http.MethodGet, "/items/:id", h.GetItem).Code,
		"a snapshot outside the gallery limits is not served")
	repo.item = &service.PelicanShowcaseItem{ID: 7, GroupID: 3, ResponseText: "<svg></svg>"}
	w = serveShowcase(http.MethodGet, "/items/:id", h.GetItem)
	require.Equal(t, http.StatusOK, w.Code)
	var envelope struct {
		Data service.PelicanShowcaseItem `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, "<svg></svg>", envelope.Data.ResponseText)

	require.Equal(t, http.StatusNotFound, serveShowcase(http.MethodDelete, "/items/:id", h.DeleteItem).Code)
	repo.deleted = true
	require.Equal(t, http.StatusOK, serveShowcase(http.MethodDelete, "/items/:id", h.DeleteItem).Code)
}

func TestPelicanShowcaseHandler_AdminSettingsRoundTrip(t *testing.T) {
	values := map[string]string{service.SettingKeyPelicanShowcaseConfig: `{"group_ids":[3],"max_items":10}`}
	h := newShowcaseHandler(values, &showcaseHandlerRepo{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/pelican-showcase/settings", nil)
	h.GetSettings(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":false,"api_enabled":true,"admin_only":false,"max_items":10,"auto_cleanup":false,"retention_days":7}}`, w.Body.String())

	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pelican-showcase/settings", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		return w
	}
	w = put(`{"enabled":true,"max_items":30,"auto_cleanup":true,"retention_days":14}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"enabled":true,"api_enabled":true,"admin_only":false,"max_items":30,"auto_cleanup":true,"retention_days":14}}`, w.Body.String())
	require.Equal(t, "true", values[service.SettingKeyPelicanShowcaseEnabled])
	require.JSONEq(t, `{"max_items":30,"auto_cleanup":true,"retention_days":14}`, values[service.SettingKeyPelicanShowcaseConfig],
		"the legacy group list is dropped on save")
	require.NotContains(t, values, service.SettingKeyPelicanShowcaseAPIEnabled, "an older client does not configure the new switch")

	w = put(`{"enabled":true,"api_enabled":false,"max_items":30,"auto_cleanup":true,"retention_days":14}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"api_enabled":false`)
	require.Equal(t, "false", values[service.SettingKeyPelicanShowcaseAPIEnabled])
	w = put(`{"enabled":true,"max_items":30,"auto_cleanup":true,"retention_days":14}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"api_enabled":false`)
	require.Equal(t, "false", values[service.SettingKeyPelicanShowcaseAPIEnabled], "an omitted API switch preserves its saved value")
	w = put(`{"enabled":true,"admin_only":true,"max_items":30,"auto_cleanup":true,"retention_days":14}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"admin_only":true`)
	require.Equal(t, "true", values[service.SettingKeyPelicanShowcaseAdminOnly])
	w = put(`{"enabled":true,"max_items":30,"auto_cleanup":true,"retention_days":14}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"admin_only":true`, "older clients preserve the saved audience")

	saved := values[service.SettingKeyPelicanShowcaseConfig]
	for _, bad := range []string{`{"max_items":101}`, `{"retention_days":91}`, `{"api_enabled":"false"}`, `{"admin_only":"false"}`, `not json`} {
		w = put(bad)
		require.Equal(t, http.StatusBadRequest, w.Code, bad)
		require.Equal(t, saved, values[service.SettingKeyPelicanShowcaseConfig], bad)
	}
}

func TestPelicanShowcaseHandler_APIDisabledPreservesJWTGallery(t *testing.T) {
	repo := &showcaseHandlerRepo{
		groups: []*service.PelicanShowcaseGroup{{ID: 3, Name: "VIP"}},
		items:  []*service.PelicanShowcaseItem{{ID: 7, GroupID: 3}},
		item:   &service.PelicanShowcaseItem{ID: 7, GroupID: 3, ResponseText: "<svg></svg>"},
	}
	h := newShowcaseHandler(map[string]string{
		service.SettingKeyPelicanShowcaseEnabled:    "true",
		service.SettingKeyPelicanShowcaseAPIEnabled: "false",
	}, repo)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pelican-showcase", nil)
	h.List(c)
	require.Equal(t, http.StatusOK, w.Code)
	var view struct {
		Data service.PelicanShowcaseView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &view))
	require.True(t, view.Data.Enabled)
	require.False(t, view.Data.APIEnabled)
	require.Len(t, view.Data.Groups, 1)
	require.Len(t, view.Data.Groups[0].Items, 1)
	detail := serveShowcase(http.MethodGet, "/items/:id", h.GetItem)
	require.Equal(t, http.StatusOK, detail.Code)
	var item struct {
		Data service.PelicanShowcaseItem `json:"data"`
	}
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &item))
	require.Equal(t, "<svg></svg>", item.Data.ResponseText)
	require.Equal(t, http.StatusForbidden, publicPelicanRequest(h, http.MethodGet, "", nil).Code)
	require.Equal(t, http.StatusForbidden, publicPelicanRequest(h, http.MethodGet, "7", nil).Code)
}
