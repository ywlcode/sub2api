package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContentModerationBanScopeHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, engine := range []string{service.ContentModerationEngineOpenAI, service.ContentModerationEngineTypeSafe} {
		t.Run(engine, func(t *testing.T) {
			settings := &moderationEngineSettings{value: fmt.Sprintf(`{"engine":%q,"auto_ban_enabled":true,"all_groups":false,"group_ids":[7],"model_filter":{"type":"include","models":["gpt-fixture"]}}`, engine)}
			newHandler := func() *ContentModerationHandler {
				return NewContentModerationHandler(service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil))
			}
			h := newHandler()
			request := func(handler *ContentModerationHandler, method, body string) *httptest.ResponseRecorder {
				t.Helper()
				router := gin.New()
				router.GET("/admin/risk-control/config", handler.GetConfig)
				router.PUT("/admin/risk-control/config", handler.UpdateConfig)
				req := httptest.NewRequest(method, "/admin/risk-control/config", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			assertScope := func(w *httptest.ResponseRecorder, scope string) {
				t.Helper()
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var result struct {
					Data service.ContentModerationConfigView `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
				require.Equal(t, scope, result.Data.BanScope)
				require.Equal(t, engine, result.Data.Engine)
				require.True(t, result.Data.AutoBanEnabled)
				require.False(t, result.Data.AllGroups, "changing the ban scope must preserve the audit scope")
				require.Equal(t, []int64{7}, result.Data.GroupIDs)
				require.Equal(t, service.ContentModerationModelFilter{Type: "include", Models: []string{"gpt-fixture"}}, result.Data.ModelFilter)
			}
			assertReloaded := func(scope string) {
				t.Helper()
				// A fresh service reads the persisted JSON, not the update-time cache.
				assertScope(request(newHandler(), http.MethodGet, ""), scope)
				var saved service.ContentModerationConfig
				require.NoError(t, json.Unmarshal([]byte(settings.value), &saved))
				require.Equal(t, scope, saved.BanScope)
			}

			assertScope(request(h, http.MethodGet, ""), service.ContentModerationBanScopeAll)
			assertScope(request(h, http.MethodPut, `{"ban_scope":"openai"}`), service.ContentModerationBanScopeOpenAI)
			assertReloaded(service.ContentModerationBanScopeOpenAI)

			// Older clients and unrelated edits must retain the saved restriction.
			for _, body := range []string{`{"email_on_hit":false}`, `{"ban_scope":null}`} {
				assertScope(request(h, http.MethodPut, body), service.ContentModerationBanScopeOpenAI)
				assertReloaded(service.ContentModerationBanScopeOpenAI)
			}
			for _, body := range []string{`{"ban_scope":"unsupported"}`, `{"ban_scope":true}`} {
				before := settings.value
				w := request(h, http.MethodPut, body)
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				require.Equal(t, before, settings.value, "invalid scope must not overwrite the saved setting")
				assertReloaded(service.ContentModerationBanScopeOpenAI)
			}
			assertScope(request(h, http.MethodPut, `{"ban_scope":"all"}`), service.ContentModerationBanScopeAll)
			assertReloaded(service.ContentModerationBanScopeAll)
		})
	}
}
