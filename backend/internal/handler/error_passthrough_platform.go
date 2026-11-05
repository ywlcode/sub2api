package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// errorPassthroughPlatform uses the selected upstream account, not the wire
// protocol or gateway handler. OpenAI-compatible endpoints serve many platforms.
func errorPassthroughPlatform(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if c.Request != nil {
		// setOpsSelectedAccount refreshes this value on each account selection.
		if platform, ok := c.Request.Context().Value(ctxkey.Platform).(string); ok {
			if platform = strings.TrimSpace(platform); platform != "" {
				return platform
			}
		}
	}
	// Some paths have only the resolved composite target or concrete group.
	// An unknown platform must not implicitly opt into OpenAI-specific rules.
	apiKey, _ := middleware.GetAPIKeyFromContext(c)
	return effectiveAPIKeyPlatform(c, apiKey)
}

// Shared final-response matching for the Responses, Chat and Messages handlers.
// Use the selected account platform even when the entry group is composite.
func matchUpstreamErrorPassthrough(c *gin.Context, rules *service.ErrorPassthroughService, status int, body []byte) (int, string, bool) {
	if rules == nil {
		return 0, "", false
	}
	rule := rules.MatchRule(errorPassthroughPlatform(c), status, body)
	if rule == nil {
		return 0, "", false
	}
	if !rule.PassthroughCode && rule.ResponseCode != nil {
		status = *rule.ResponseCode
	}
	message := service.ExtractUpstreamErrorMessage(body)
	if !rule.PassthroughBody && rule.CustomMessage != nil {
		message = *rule.CustomMessage
	}
	if rule.SkipMonitoring {
		c.Set(service.OpsSkipPassthroughKey, true)
	}
	return status, message, true
}
