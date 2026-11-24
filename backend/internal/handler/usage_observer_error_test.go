//go:build unit

package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type observerErrorRepo struct {
	service.OpsRepository
	filter *service.OpsErrorLogFilter
	detail service.OpsErrorLogDetail
	calls  int
}

func (r *observerErrorRepo) ListErrorLogs(_ context.Context, f *service.OpsErrorLogFilter) (*service.OpsErrorLogList, error) {
	r.calls++
	r.filter = f
	return &service.OpsErrorLogList{Errors: []*service.OpsErrorLog{&r.detail.OpsErrorLog}, Total: 1, Page: 1, PageSize: 20}, nil
}
func (r *observerErrorRepo) GetErrorLogByID(context.Context, int64) (*service.OpsErrorLogDetail, error) {
	r.calls++
	return &r.detail, nil
}

func ownErrorsRouter(repo *observerErrorRepo, role, enabled string) *gin.Engine {
	settings := service.NewSettingService(&settingHandlerPublicRepoStub{values: map[string]string{service.SettingKeyAllowUserViewErrorRequests: enabled}}, nil)
	ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := NewUsageHandler(nil, nil, ops, settings)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 42})
		c.Set(string(middleware.ContextKeyUserRole), role)
	})
	router.GET("/usage/errors", h.ListErrors)
	router.GET("/usage/errors/:id", h.GetErrorDetail)
	return router
}

func TestOwnUsageErrorsKeepCyberEvidenceOutOfUserResponses(t *testing.T) {
	owner := int64(42)
	for _, role := range []string{service.RoleUser, service.RoleObserver} {
		for _, tc := range []struct {
			kind, message, body string
			status              int
		}{
			{"cyber_policy", "cyber_policy: Excel BPS upstream failure: cyber_policy; request was not replayed; session_identity_source=header_body", `{"error":{"code":"cyber_policy","message":"Excel BPS upstream failure: cyber_policy"}}`, 200},
			{"cyber_policy_session_blocked", "request rejected locally by session block", "session_block_key=private", 403},
			{"cyber_session_identity_rejected", "request rejected before account selection; session_identity_status=conflict", "", 400},
		} {
			t.Run(role+"/"+tc.kind, func(t *testing.T) {
				repo := &observerErrorRepo{detail: service.OpsErrorLogDetail{
					OpsErrorLog: service.OpsErrorLog{ID: 7, UserID: &owner, Phase: "request", Type: tc.kind, StatusCode: tc.status, Message: tc.message},
					ErrorBody:   tc.body,
				}}
				router := ownErrorsRouter(repo, role, "true")
				for path, record := range map[string]string{"/usage/errors": "data.items.0", "/usage/errors/7": "data"} {
					w := usageGET(router, path)
					require.Equal(t, 200, w.Code)
					row := gjson.Get(w.Body.String(), record)
					require.EqualValues(t, tc.status, row.Get("status_code").Int())
					if tc.kind == "cyber_session_identity_rejected" {
						require.Contains(t, row.Get("message").String(), "session_id")
						require.NotEqual(t, service.CyberPolicyClientMessage, row.Get("message").String())
					} else {
						require.Equal(t, service.CyberPolicyClientMessage, row.Get("message").String())
					}
					require.Empty(t, row.Get("error_body").String())
					for _, private := range []string{"Excel BPS", "request was not replayed", "session_identity_source=", "session_identity_status=", "session_block_key=", tc.message} {
						require.NotContains(t, w.Body.String(), private)
					}
				}
				require.Equal(t, tc.message, repo.detail.Message)
				require.Equal(t, tc.body, repo.detail.ErrorBody)
			})
		}
	}
}

func TestObserverUsageErrorsScopeAndVisibility(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			owner := int64(42)
			repo := &observerErrorRepo{detail: service.OpsErrorLogDetail{OpsErrorLog: service.OpsErrorLog{ID: 7, UserID: &owner, AccountName: "own-account"}, APIKeyPrefix: "secret-prefix", ErrorBody: "own-error"}}
			router := ownErrorsRouter(repo, service.RoleObserver, enabled)
			w := usageGET(router, "/usage/errors?user_id=999&account_id=8&group_id=9&phase=upstream&status_code=502&start_date=2026-09-01&end_date=2026-09-02")
			if enabled == "false" {
				require.Equal(t, 403, w.Code)
				require.Equal(t, 403, usageGET(router, "/usage/errors/7").Code)
				require.Zero(t, repo.calls)
				return
			}
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, int64(42), *repo.filter.UserID)
			require.Equal(t, int64(8), *repo.filter.AccountID)
			require.Equal(t, int64(9), *repo.filter.GroupID)
			require.Equal(t, "upstream", repo.filter.Phase)
			require.Equal(t, []int{502}, repo.filter.StatusCodes)
			require.NotNil(t, repo.filter.StartTime)
			require.NotContains(t, w.Body.String(), "secret-prefix")
			w = usageGET(router, "/usage/errors/7")
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "own-error")
			require.NotContains(t, w.Body.String(), "secret-prefix")
			owner = 999
			require.Equal(t, 404, usageGET(router, "/usage/errors/7").Code)
		})
	}
}
