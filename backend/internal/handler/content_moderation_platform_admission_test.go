package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type platformAdmissionRepo struct {
	service.ContentModerationRepository
	blocked atomic.Bool
	err     error
}

func (r *platformAdmissionRepo) IsOpenAIBlocked(context.Context, int64) (bool, error) {
	return r.blocked.Load(), r.err
}

func platformAdmissionHandler(r *platformAdmissionRepo) *OpenAIGatewayHandler {
	return &OpenAIGatewayHandler{
		contentModerationService: service.NewContentModerationService(nil, r, nil, nil, nil, nil, nil, nil),
		securityAuditCoordinator: securityaudit.NewCoordinator(nil, nil),
	}
}

func TestSecurityPlatformAdmissionHTTPResolvedPlatformAndRelease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, group, resolved, forced string
		status                        int
	}{
		{"OpenAI", service.PlatformOpenAI, "", "", 403},
		{"Claude compatible endpoint", service.PlatformAnthropic, "", "", 200},
		{"Gemini", service.PlatformGemini, "", "", 200},
		{"Grok", service.PlatformGrok, "", "", 200},
		{"TypeSafe", service.PlatformTypeSafe, "", "", 200},
		{"mixed OpenAI", service.PlatformComposite, service.PlatformOpenAI, "", 403},
		{"mixed Claude alias", service.PlatformComposite, service.PlatformAnthropic, "", 200},
		{"unresolved mixed pool", service.PlatformComposite, "", "", 403},
		{"forced antigravity", service.PlatformOpenAI, "", service.PlatformAntigravity, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &platformAdmissionRepo{}
			repo.blocked.Store(true)
			h := platformAdmissionHandler(repo)
			key := &service.APIKey{ID: 11, UserID: 7, User: &service.User{ID: 7, Status: service.StatusActive}, Group: &service.Group{ID: 3, Platform: tc.group}}
			router := gin.New()
			if tc.forced != "" {
				router.Use(middleware2.ForcePlatform(tc.forced))
			}
			router.Use(func(c *gin.Context) {
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				if tc.resolved != "" {
					c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), tc.resolved))
				}
			})
			upstreamCalls := 0
			router.POST("/v1/responses", h.ContentModerationPlatformAdmission, func(c *gin.Context) { upstreamCalls++; c.Status(http.StatusOK) })
			call := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"custom-alias","input":"benign"}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Upgrade", "websocket") // A header on HTTP POST must not bypass the gate.
				router.ServeHTTP(w, req)
				return w
			}
			w := call()
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 403 {
				require.Zero(t, upstreamCalls)
				key.ID++
				key.Group.ID++
				require.Equal(t, 403, call().Code, "another key or group for the same user cannot bypass the ban")
			} else {
				require.Equal(t, 1, upstreamCalls)
			}
			require.Equal(t, service.StatusActive, key.User.Status)
			repo.blocked.Store(false)
			require.Equal(t, 200, call().Code)
		})
	}
}

func TestSecurityPlatformAdmissionWebSocketRechecksEachTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &platformAdmissionRepo{}
	h := platformAdmissionHandler(repo)
	key := &service.APIKey{ID: 11, UserID: 7, Group: &service.Group{Platform: service.PlatformComposite}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenAI))
	subject := middleware2.AuthSubject{UserID: 7}
	payload := []byte(`{"type":"response.create","model":"alias","input":"benign"}`)
	first := h.checkSecurityAuditStage(c, nil, key, subject, "openai_responses", "alias", payload, "first_turn")
	require.True(t, first.AllowNextStage)
	// A ban issued after connection establishment must affect a later frame,
	// even when an HTTP completion marker is present on the shared context.
	c.Set(securityAuditCompletedContextKey, true)
	repo.blocked.Store(true)
	next := h.checkSecurityAuditStage(c, nil, key, subject, "openai_responses", "alias", payload, "subsequent_turn")
	require.False(t, next.AllowNextStage)
	require.Equal(t, "openai_access_restricted", next.ErrorCode)
	repo.blocked.Store(false)
	require.True(t, h.checkSecurityAuditStage(c, nil, key, subject, "openai_responses", "alias", payload, "subsequent_turn").AllowNextStage)
}
