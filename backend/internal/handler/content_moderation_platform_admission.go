package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const moderationPlatformCheckedKey = "content_moderation.platform_checked"

type moderationPlatformCheck struct {
	userID   int64
	platform string
}

// Usage reads only the authenticated key and its owner. Match the registered
// route and method so another endpoint ending in /usage still requires admission.
func isOwnGatewayUsage(c *gin.Context) bool {
	return c.Request.Method == http.MethodGet && (c.FullPath() == "/v1/usage" || c.FullPath() == "/antigravity/v1/usage")
}

// Load the access decision before a request can be proxied to another pod or
// a WebSocket can be upgraded pending first-frame group selection.
func (h *OpenAIGatewayHandler) ContentModerationPreAdmission(c *gin.Context) {
	if isOwnGatewayUsage(c) {
		c.Next()
		return
	}
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if h != nil && h.contentModerationService != nil && ok && key != nil {
		access, err := h.contentModerationService.ReadPlatformAccess(c.Request.Context(), key.UserID)
		if err != nil {
			h.platformAdmissionError(c, moderationAccessDecision(err))
			c.Abort()
			return
		}
		c.Set(moderationAccessKey, moderationRequestAccess{key.UserID, access})
	}
	c.Next()
}

const moderationAccessKey = "content_moderation.platform_access"

type moderationRequestAccess struct {
	userID int64
	access service.ModerationPlatformAccess
}

// ContentModerationPlatformAdmission is installed after group selection and composite
// routing. It also covers routes that do not invoke the content audit engine.
func (h *OpenAIGatewayHandler) ContentModerationPlatformAdmission(c *gin.Context) {
	if h == nil || isOwnGatewayUsage(c) || middleware2.ResponsesWebSocketGroupPending(c) {
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
	ctx := c.Request.Context()
	var err error
	raw, _ := c.Get(moderationAccessKey)
	if snapshot, ok := raw.(moderationRequestAccess); cache && ok && snapshot.userID == userID {
		err = snapshot.access.Check(ctx, platform)
	} else {
		err = svc.CheckPlatformAccess(ctx, userID, platform)
	}
	if err != nil {
		return moderationAccessDecision(err)
	}
	if cache {
		c.Set(moderationPlatformCheckedKey, check)
	}
	return nil
}

func moderationAccessDecision(err error) *securityaudit.Decision {
	kind := securityaudit.DecisionBlock
	if infraerrors.Code(err) >= http.StatusInternalServerError {
		kind = securityaudit.DecisionUnavailable
	}
	return &securityaudit.Decision{Kind: kind, HTTPStatus: infraerrors.Code(err), ErrorCode: infraerrors.Reason(err), ClientMessage: infraerrors.Message(err)}
}

// Voice and Live sideband carry both JSON and binary frames. Explicit control
// events must see fresh authority. Continuous input can trigger server-side VAD
// without a client commit, so even one uninterrupted stream needs bounded checks.
const voiceStreamRecheckInterval = time.Second

type moderatedWebSocket struct {
	service.GatewayClientWebSocket
	moderation     *service.ContentModerationService
	userID         int64
	platform       string
	nextChunkCheck time.Time   // owned by the client reader
	turnEnded      atomic.Bool // set by the upstream reader
}

func (h *OpenAIGatewayHandler) moderatedWebSocket(conn service.GatewayClientWebSocket, userID int64, platform string) service.GatewayClientWebSocket {
	if h.contentModerationService == nil {
		return conn
	}
	return &moderatedWebSocket{GatewayClientWebSocket: conn, moderation: h.contentModerationService, userID: userID, platform: platform}
}

func (ws *moderatedWebSocket) Read(ctx context.Context) (coderws.MessageType, []byte, error) {
	kind, payload, err := ws.GatewayClientWebSocket.Read(ctx)
	if err != nil {
		return kind, nil, err
	}
	chunk := ws.platform == service.PlatformGrok && grokVoiceInputChunk(kind, payload)
	if chunk && !ws.turnEnded.Swap(false) && time.Now().Before(ws.nextChunkCheck) {
		return kind, payload, nil
	}
	if err := ws.moderation.CheckPlatformAccess(ctx, ws.userID, ws.platform); err != nil {
		decision := moderationAccessDecision(err)
		return kind, nil, service.NewOpenAIWSClientCloseError(securityAuditWSCloseStatus(decision), securityAuditWSCloseReason(decision), err)
	}
	if chunk {
		ws.nextChunkCheck = time.Now().Add(voiceStreamRecheckInterval)
	} else {
		ws.nextChunkCheck = time.Time{}
	}
	return kind, payload, nil
}

func grokVoiceInputChunk(kind coderws.MessageType, payload []byte) bool {
	if kind == coderws.MessageBinary && !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
		return true
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "input_audio_buffer.append", "text.delta":
		return true
	}
	return false
}

func (ws *moderatedWebSocket) Write(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	if ws.platform == service.PlatformGrok && kind == coderws.MessageText {
		switch gjson.GetBytes(payload, "type").String() {
		case "input_audio_buffer.speech_stopped", "input_audio_buffer.committed", "response.done", "audio.done":
			ws.turnEnded.Store(true)
		}
	}
	return ws.GatewayClientWebSocket.Write(ctx, kind, payload)
}

func closeModeratedWebSocket(conn *coderws.Conn, err error) bool {
	var closeErr *service.OpenAIWSClientCloseError
	if !errors.As(err, &closeErr) {
		return false
	}
	_ = conn.Close(closeErr.StatusCode(), closeErr.Reason())
	return true
}
