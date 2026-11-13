package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newCyberPlatformTestHandler(t *testing.T) (*OpenAIGatewayHandler, *contentModerationHandlerTestRepo) {
	t.Helper()
	settings := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:                "true",
		service.SettingKeyCyberSessionBlockEnabled:          "true",
		service.SettingKeyCyberSessionIdentityStrictEnabled: "true",
	}}
	settingSvc := service.NewSettingService(settings, nil)
	logs := &contentModerationHandlerTestRepo{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gw := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, testutil.NewRedisGatewayCache(t), cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, nil, nil, &service.DeferredService{}, nil, nil, nil, nil, nil, settingSvc, nil)
	return &OpenAIGatewayHandler{gatewayService: gw, contentModerationService: service.NewContentModerationService(settings, logs, nil, nil, nil, nil, nil, nil)}, logs
}

func cyberPlatformContext(platform, session string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), platform))
	if session != "" {
		c.Request.Header.Set("session_id", session)
	}
	return c, rec
}

func TestCyberPlatformAuditAndSessionIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupOpsErrorLogTestQueue(t, 32)
	h, logs := newCyberPlatformTestHandler(t)
	// A composite group, and a misleading public alias: the selected account and
	// resolved target, not the protocol or model prefix, determine the platform.
	key := &service.APIKey{ID: 7, UserID: 8, Group: &service.Group{Platform: service.PlatformComposite}, User: &service.User{ID: 8}}
	body := []byte(`{"model":"public-alias","input":"synthetic audit fixture"}`)
	platforms := []string{service.PlatformOpenAI, service.PlatformDeepseek, service.PlatformGrok, service.PlatformAnthropic, service.PlatformGemini, service.PlatformMiniMax, service.PlatformKimi, service.PlatformZhipu}
	for i, platform := range platforms {
		t.Run(platform, func(t *testing.T) {
			session := fmt.Sprintf("session-%d", i)
			c, _ := cyberPlatformContext(service.PlatformOpenAI, session)
			service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "original upstream reason", Body: `{"error":{"code":"cyber_policy","message":"original upstream reason","api_key":"sk-should-be-redacted"}}`, UpstreamStatus: 400})
			h.opsService = service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			h.recordCyberPolicyIfMarked(c, key, &service.Account{ID: 9, Platform: platform}, nil, "public-alias", false, body, service.ChannelUsageFields{}, "")
			select {
			case job := <-opsErrorLogQueue:
				require.Equal(t, platform, job.entry.Platform)
				require.Equal(t, "cyber_policy", job.entry.ErrorType)
				require.Contains(t, job.entry.ErrorBody, "original upstream reason")
				require.NotContains(t, job.entry.ErrorBody, "sk-should-be-redacted")
			case <-time.After(3 * time.Second):
				t.Fatal("missing platform Cyber ops evidence")
			}
			entries := logs.logSnapshot()
			require.Len(t, entries, i+1)
			entry := entries[i]
			require.Equal(t, platform, entry.Provider)
			require.Equal(t, service.ContentModerationActionCyberPolicy, entry.Action)
			require.Contains(t, entry.Error, "original upstream reason")
			require.NotContains(t, entry.Error, "sk-should-be-redacted")
			if platform != service.PlatformOpenAI {
				require.Equal(t, service.ContentModerationModeCyberLogOnly, entry.Mode)
				require.False(t, entry.AutoBanned)
			}
			// Avoid extra ops entries from the local rejection checks below.
			h.opsService = nil
			for _, target := range platforms {
				for _, format := range []cyberSessionBlockFormat{cyberBlockFormatResponses, cyberBlockFormatChat, cyberBlockFormatAnthropic} {
					next, rec := cyberPlatformContext(target, session)
					blocked := h.rejectIfCyberSessionBlocked(next, key, body, "gpt-misleading-alias", format)
					want := platform == service.PlatformOpenAI && target == service.PlatformOpenAI
					require.Equal(t, want, blocked, "hit=%s next=%s format=%d", platform, target, format)
					if want {
						require.Equal(t, http.StatusForbidden, rec.Code)
						require.Equal(t, service.CyberPolicyClientMessage, gjson.Get(rec.Body.String(), "error.message").String())
					}
				}
			}
		})
	}
}

func TestCyberPlatformStrictIdentityOnlyAppliesToOpenAI(t *testing.T) {
	h, _ := newCyberPlatformTestHandler(t)
	key := &service.APIKey{ID: 7, Group: &service.Group{Platform: service.PlatformComposite}}
	c, rec := cyberPlatformContext(service.PlatformDeepseek, "")
	require.False(t, h.rejectIfCyberSessionBlocked(c, key, []byte(`{"input":"hi"}`), "gpt-alias", cyberBlockFormatResponses))
	require.Equal(t, http.StatusOK, rec.Code)
	c, rec = cyberPlatformContext(service.PlatformOpenAI, "")
	require.True(t, h.rejectIfCyberSessionBlocked(c, key, []byte(`{"input":"hi"}`), "deepseek-alias", cyberBlockFormatResponses))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCyberLocalBlockAfterSSEHeartbeatHasFixedTerminal(t *testing.T) {
	h, _ := newCyberPlatformTestHandler(t)
	key := &service.APIKey{ID: 7}
	c, rec := cyberPlatformContext(service.PlatformOpenAI, "blocked")
	body := []byte(`{"input":"hi"}`)
	identity := service.ResolveCyberSessionIdentity(key.ID, c, body)
	h.gatewayService.MarkCyberSessionBlockedForRequest(context.Background(), service.PlatformOpenAI, key.ID, identity, body)
	// Compact keepalive commits the response before local admission finishes.
	service.MarkOpenAICompactClientStream(c)
	stop := service.StartOpenAICompactSSEKeepalive(c, time.Millisecond)
	defer stop()
	require.Eventually(t, c.Writer.Written, time.Second, time.Millisecond)
	require.True(t, h.rejectIfCyberSessionBlocked(c, key, body, "gpt-5", cyberBlockFormatResponses))
	require.Contains(t, rec.Body.String(), "response.failed")
	require.Contains(t, rec.Body.String(), service.CyberPolicyClientMessage)
}

func TestCyberLocalBlockAfterQueueHeartbeatAllProtocols(t *testing.T) {
	h, _ := newCyberPlatformTestHandler(t)
	key := &service.APIKey{ID: 7}
	for _, format := range []cyberSessionBlockFormat{cyberBlockFormatResponses, cyberBlockFormatChat, cyberBlockFormatAnthropic} {
		c, rec := cyberPlatformContext(service.PlatformOpenAI, "queue-blocked")
		body := []byte(`{"input":"hi"}`)
		identity := service.ResolveCyberSessionIdentity(key.ID, c, body)
		h.gatewayService.MarkCyberSessionBlockedForRequest(context.Background(), service.PlatformOpenAI, key.ID, identity, body)
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString(": ping\n\n")
		c.Writer.Flush()
		require.True(t, h.rejectIfCyberSessionBlocked(c, key, body, "gpt-5", format))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), service.CyberPolicyClientMessage)
		switch format {
		case cyberBlockFormatResponses:
			require.Contains(t, rec.Body.String(), "event: response.failed\n")
		case cyberBlockFormatChat:
			require.Contains(t, rec.Body.String(), "data: [DONE]\n\n")
		default:
			require.Contains(t, rec.Body.String(), "event: error\n")
		}
	}
}

func TestCyberOpsEvidenceRedactsTextAndKeepsOriginalStatus(t *testing.T) {
	const secret = "sk-syntheticsecret1234567890"
	mark := &service.CyberPolicyMark{
		Message:        "policy denied Authorization: Bearer " + secret,
		Body:           `{"error":{"code":"cyber_policy","message":"Bearer ` + secret,
		UpstreamStatus: http.StatusBadRequest,
	}
	entry := buildCyberPolicyOpsErrorEntry(cyberPolicyOpsErrorMeta{ClientStatus: http.StatusForbidden}, mark)
	require.NotContains(t, entry.ErrorMessage, secret)
	require.NotContains(t, entry.ErrorBody, secret, "truncated JSON must still redact credentials")
	require.Contains(t, entry.ErrorMessage, "policy denied")
	require.Contains(t, mark.Message, secret, "redaction must not mutate the original evidence")
	require.Equal(t, http.StatusForbidden, entry.StatusCode)
	require.Equal(t, http.StatusBadRequest, *entry.UpstreamStatusCode)
}
