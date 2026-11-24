package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
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

type combinedPlatformAdmissionRepo struct {
	service.ContentModerationRepository
	all    bool
	openAI bool
	err    error
	reads  int
}

func (r *combinedPlatformAdmissionRepo) ModerationPlatformAccess(context.Context, int64) (bool, bool, error) {
	r.reads++
	return r.all, r.openAI, r.err
}

func TestSecurityPlatformAdmissionOwnUsageExactRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		exempt       bool
	}{
		{"GET", "/v1/usage", true},
		{"GET", "/antigravity/v1/usage", true},
		{"POST", "/v1/usage", false},
		{"HEAD", "/antigravity/v1/usage", false},
		{"GET", "/other/v1/usage", false},
		{"GET", "/v1/usage/export", false},
		{"GET", "/v1/models", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			repo := &combinedPlatformAdmissionRepo{all: true, openAI: true}
			h := &OpenAIGatewayHandler{contentModerationService: service.NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)}
			for _, gate := range []gin.HandlerFunc{h.ContentModerationPreAdmission, h.ContentModerationPlatformAdmission} {
				router := gin.New()
				router.Use(func(c *gin.Context) {
					c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{UserID: 8, Group: &service.Group{Platform: service.PlatformOpenAI}})
				})
				router.Handle(tc.method, tc.path, gate, func(c *gin.Context) { c.Status(http.StatusNoContent) })
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
				if tc.exempt {
					require.Equal(t, http.StatusNoContent, w.Code)
					require.Zero(t, repo.reads)
				} else {
					require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestSecurityPlatformAdmissionReadsOncePerHTTPAndFreshForTurns(t *testing.T) {
	r := &combinedPlatformAdmissionRepo{}
	mod := service.NewContentModerationService(nil, r, nil, nil, nil, nil, nil, nil)
	h := &OpenAIGatewayHandler{contentModerationService: mod}
	key := &service.APIKey{UserID: 8, Group: &service.Group{Platform: service.PlatformComposite}}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(middleware2.ContextKeyAPIKey), key) })
	router.POST("/v1/messages", h.ContentModerationPreAdmission, func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformAnthropic))
	}, h.ContentModerationPlatformAdmission, func(c *gin.Context) {
		require.Nil(t, checkModerationPlatformAccess(c, mod, 8, service.PlatformAnthropic, true))
		require.Equal(t, 1, r.reads)
		r.all = true
		denied := checkModerationPlatformAccess(c, mod, 8, service.PlatformAnthropic, false)
		require.Equal(t, "all_platforms_access_restricted", denied.ErrorCode)
		c.Status(http.StatusNoContent)
	})
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		return w
	}
	r.openAI = true
	require.Equal(t, http.StatusNoContent, call().Code)
	require.Equal(t, http.StatusForbidden, call().Code)
	r.err = context.DeadlineExceeded
	require.Equal(t, http.StatusServiceUnavailable, call().Code)
}

type voiceFrameFixture struct {
	service.GatewayClientWebSocket
	kind    coderws.MessageType
	payload []byte
}

func (f *voiceFrameFixture) Read(context.Context) (coderws.MessageType, []byte, error) {
	return f.kind, f.payload, nil
}
func (*voiceFrameFixture) Write(context.Context, coderws.MessageType, []byte) error { return nil }

func TestSecurityPlatformAdmissionStreamingFrames(t *testing.T) {
	for _, tc := range []struct {
		name, platform  string
		kind            coderws.MessageType
		chunk, boundary string
	}{
		{"realtime binary", service.PlatformGrok, coderws.MessageBinary, "pcm", "response.create"},
		{"realtime JSON", service.PlatformGrok, coderws.MessageText, `{"type":"input_audio_buffer.append","audio":"AA"}`, "input_audio_buffer.commit"},
		{"tts", service.PlatformGrok, coderws.MessageText, `{"type":"text.delta","delta":"test"}`, "text.done"},
		{"live sideband", service.PlatformOpenAI, coderws.MessageText, `{"type":"response.create"}`, "response.create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &combinedPlatformAdmissionRepo{}
			h := &OpenAIGatewayHandler{contentModerationService: service.NewContentModerationService(nil, r, nil, nil, nil, nil, nil, nil)}
			f := &voiceFrameFixture{kind: tc.kind, payload: []byte(tc.chunk)}
			ws := h.moderatedWebSocket(f, 7, tc.platform)
			ctx := context.Background()
			for i := 0; i < 25; i++ {
				_, body, err := ws.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, f.payload, body)
			}
			if tc.platform == service.PlatformGrok {
				require.Equal(t, 1, r.reads)
				require.NoError(t, ws.Write(ctx, coderws.MessageText, []byte(`{"type":"response.done"}`)))
				r.all = true
				_, payload, err := ws.Read(ctx)
				require.Empty(t, payload)
				require.ErrorContains(t, err, "all_platforms_access_restricted")
				r.all = false
				// No explicit turn boundary during continuous audio: expiry must
				// also force a check without depending on wall-clock sleeps.
				ws.(*moderatedWebSocket).nextChunkCheck = time.Now().Add(-time.Second)
				r.err = context.DeadlineExceeded
				_, payload, err = ws.Read(ctx)
				require.Empty(t, payload)
				require.ErrorContains(t, err, "security_platform_check_unavailable")
				r.err = nil
			}
			r.all = true
			f.kind = coderws.MessageText
			f.payload = []byte(`{"type":"` + tc.boundary + `"}`)
			_, payload, err := ws.Read(ctx)
			require.Empty(t, payload)
			require.ErrorContains(t, err, "all_platforms_access_restricted")
		})
	}
}

type socketBanRepo struct {
	contentModerationHandlerTestRepo
	blocked atomic.Bool
}

func (r *socketBanRepo) ModerationPlatformAccess(context.Context, int64) (bool, bool, error) {
	return r.blocked.Load(), false, nil
}

func TestSecurityPlatformAdmissionDeniesConnectedResponsesNextTurn(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			repo := &socketBanRepo{}
			h, up := newIssue600NetworkHarness(t, mode, "none", false, nil, issue600FixtureOptions{configureHandler: func(h *OpenAIGatewayHandler) {
				h.contentModerationService = service.NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
				h.securityAuditCoordinator = securityaudit.NewCoordinator(nil, nil)
			}})
			const payload = `{"type":"response.create","model":"gpt-5.1","input":"test"}`
			issue600Send(t, h, payload, 1, false)
			repo.blocked.Store(true)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(payload)))
			_, body, err := h.clientConn.Read(ctx)
			if err != nil {
				require.Equal(t, coderws.StatusCode(4403), coderws.CloseStatus(err), err)
			} else {
				require.Contains(t, string(body), "all_platforms_access_restricted")
			}
			issue600Finish(t, h, nil)
			frames, _ := up.snapshot()
			require.Len(t, frames, 1)
		})
	}
}
