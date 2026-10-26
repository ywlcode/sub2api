package handler

import (
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const moderationPlatformCheckedKey = "content_moderation.platform_checked"

type moderationPlatformCheck struct {
	userID   int64
	platform string
}

// ContentModerationPlatformAdmission is installed after group selection and composite
// routing. It also covers routes that do not invoke the content audit engine.
func (h *OpenAIGatewayHandler) ContentModerationPlatformAdmission(c *gin.Context) {
	if h == nil {
		c.Next()
		return
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		c.Next()
		return
	}
	subject := middleware2.AuthSubject{UserID: apiKey.UserID}
	input := buildContentModerationInput(c, apiKey, subject, "", "", nil)
	// A composite WebSocket resolves its target from the first frame. Admission
	// is repeated before that frame and each subsequent frame reaches upstream.
	if c.Request.Method == http.MethodGet && strings.HasSuffix(c.FullPath(), "/responses") && strings.EqualFold(c.GetHeader("Upgrade"), "websocket") && input.Provider == service.PlatformComposite {
		c.Next()
		return
	}
	if decision := checkModerationPlatformAccess(c, h.contentModerationService, subject.UserID, input.Provider, true); decision != nil {
		h.platformAdmissionError(c, decision)
		c.Abort()
		return
	}

	c.Next()
}

func (h *OpenAIGatewayHandler) platformAdmissionError(c *gin.Context, decision *securityaudit.Decision) {
	path := c.Request.URL.Path
	switch {
	case strings.Contains(path, "/v1beta/"):
		googleSecurityAuditError(c, decision)
	case strings.HasSuffix(path, "/messages"), strings.HasSuffix(path, "/messages/count_tokens"):
		h.anthropicSecurityAuditError(c, decision)
	default:
		h.openAISecurityAuditError(c, decision)
	}
}

func checkModerationPlatformAccess(c *gin.Context, svc *service.ContentModerationService, userID int64, platform string, cache bool) *securityaudit.Decision {
	if svc == nil {
		return nil
	}
	check := moderationPlatformCheck{userID: userID, platform: platform}
	if checked, exists := c.Get(moderationPlatformCheckedKey); cache && exists && checked == check {
		return nil
	}
	if err := svc.CheckPlatformAccess(c.Request.Context(), userID, platform); err != nil {
		kind := securityaudit.DecisionBlock
		if infraerrors.Code(err) >= http.StatusInternalServerError {
			kind = securityaudit.DecisionUnavailable
		}
		return &securityaudit.Decision{Kind: kind, HTTPStatus: infraerrors.Code(err), ErrorCode: infraerrors.Reason(err), ClientMessage: infraerrors.Message(err)}
	}
	if cache {
		c.Set(moderationPlatformCheckedKey, check)
	}
	return nil
}
