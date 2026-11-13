package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Use the resolved route (including aliases/composite groups), never the API
// protocol or the model's name. Ungrouped OpenAI endpoints retain their default.
func cyberSessionRequestPlatform(c *gin.Context, apiKey *service.APIKey) string {
	if platform := effectiveAPIKeyPlatform(c, apiKey); platform != "" {
		return platform
	}
	return service.PlatformOpenAI
}

func (h *OpenAIGatewayHandler) cyberPolicyLogOnly(c *gin.Context, apiKey *service.APIKey) bool {
	return h != nil && c != nil && c.Request != nil && h.gatewayService.CyberPolicyLogOnly(c.Request.Context(), apiKey)
}

// Both HTTP and WebSocket admission skip existing blocks for trusted users.
// Session identity is still resolved and observed by the caller.
// findBlockedCyberSessionForRequest also uses the content fingerprint fallback
// when the client supplied no explicit session identity, so an identical retry
// of a cyber-blocked request is rejected before account selection.
func (h *OpenAIGatewayHandler) findBlockedCyberSessionForRequest(c *gin.Context, apiKey *service.APIKey, identity service.CyberSessionIdentityResolution, body []byte) string {
	if apiKey == nil || h.cyberPolicyLogOnly(c, apiKey) {
		return ""
	}
	return h.gatewayService.FindCyberSessionBlockedForIdentityAndBody(c.Request.Context(), cyberSessionRequestPlatform(c, apiKey), apiKey.ID, identity, body)
}
