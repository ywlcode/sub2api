package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
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
