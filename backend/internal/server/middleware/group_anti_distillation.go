package middleware

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type groupAntiDistillationChecker func(context.Context, *service.APIKey) (time.Duration, error)

func GroupAntiDistillation(check groupAntiDistillationChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if ResponsesWebSocketGroupPending(c) {
			c.Set("api_key.responses_ws_anti_distillation", check)
			c.Next()
			return
		}
		key, ok := GetAPIKeyFromContext(c)
		if !ok || key == nil || key.Group == nil || key.Group.Platform != service.PlatformOpenAI || check == nil || !isAntiDistillationGeneration(c) {
			c.Next()
			return
		}
		ctx := service.WithGroupAntiDistillationRequestStart(c.Request.Context(), time.Now())
		boundedCheck := func(ctx context.Context) (time.Duration, error) {
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return check(bounded, key)
		}
		remaining, err := boundedCheck(ctx)
		if err != nil {
			slog.Warn("group anti-distillation admission unavailable", "group_id", key.Group.ID)
		}
		if err == nil && remaining > 0 {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
			c.Header("Retry-After", strconv.Itoa(int(math.Ceil(remaining.Seconds()))))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
				"type": "rate_limit_error", "code": "group_anti_distillation_paused",
				"message": "此分组的零缓存保护已暂停新请求，请按 Retry-After 等待恢复；此信号不代表已认定蒸馏。",
			}})
			return
		}
		ctx = service.WithGroupAntiDistillationAdmission(ctx, boundedCheck)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func isAntiDistillationGeneration(c *gin.Context) bool {
	if c.Request == nil {
		return false
	}
	path := strings.TrimSuffix(c.Request.URL.Path, "/")
	if c.Request.Method == http.MethodGet {
		return strings.HasSuffix(path, "/responses")
	}
	if c.Request.Method != http.MethodPost {
		return false
	}
	return service.IsAPIKeyModelPolicyPath(path) && !strings.HasSuffix(path, "/count_tokens")
}

// The selected first-frame group owns the same ongoing protection used by
// HTTP; no policy from the provisional handshake group survives selection.
func bindResponsesWSAntiDistillation(c *gin.Context, key *service.APIKey) error {
	value, _ := c.Get("api_key.responses_ws_anti_distillation")
	check, _ := value.(groupAntiDistillationChecker)
	if check == nil || key.Group == nil || key.Group.Platform != service.PlatformOpenAI {
		return nil
	}
	ctx := service.WithGroupAntiDistillationRequestStart(c.Request.Context(), time.Now())
	boundedCheck := func(ctx context.Context) (time.Duration, error) {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return check(bounded, key)
	}
	remaining, err := boundedCheck(ctx)
	if err != nil {
		slog.Warn("group anti-distillation admission unavailable", "group_id", key.Group.ID)
	}
	if err == nil && remaining > 0 {
		return infraerrors.New(http.StatusTooManyRequests, "group_anti_distillation_paused", "Group protection has paused new requests; please retry later")
	}
	c.Request = c.Request.WithContext(service.WithGroupAntiDistillationAdmission(ctx, boundedCheck))
	return nil
}
