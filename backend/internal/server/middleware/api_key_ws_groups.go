package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const responsesWSGroupSelectionKey = "api_key.responses_ws_group_selection"

type responsesWSGroupSelection struct {
	keys          *service.APIKeyService
	subscriptions *service.SubscriptionService
	cfg           *config.Config
	original      *service.APIKey
	selected      *service.APIKey
}

func isMultiGroupResponsesWSUpgrade(c *gin.Context, key *service.APIKey) bool {
	return key != nil && len(key.ConfiguredGroupIDs()) > 1 && isResponsesWebSocketRoute(c) &&
		strings.EqualFold(strings.TrimSpace(c.GetHeader("Upgrade")), "websocket") &&
		strings.Contains(strings.ToLower(c.GetHeader("Connection")), "upgrade")
}

func responsesWSSelection(c *gin.Context) *responsesWSGroupSelection {
	value, _ := c.Get(responsesWSGroupSelectionKey)
	selection, _ := value.(*responsesWSGroupSelection)
	return selection
}

// ResponsesWebSocketGroupPending means identity is authenticated, but the
// actual model and its group policy must still be resolved from the first frame.
func ResponsesWebSocketGroupPending(c *gin.Context) bool {
	selection := responsesWSSelection(c)
	return selection != nil && selection.selected == nil
}

// responsesWSFrameKey keeps frame checks bound to the employee group even when
// later composite routing installs a different upstream group in the context.
func responsesWSFrameKey(c *gin.Context, fallback *service.APIKey) *service.APIKey {
	if selection := responsesWSSelection(c); selection != nil && selection.selected != nil {
		return selection.selected
	}
	return fallback
}

// ResolveResponsesWebSocketGroup runs before any first-frame policy, billing,
// account admission or upstream write. Subsequent turns stay pinned to this
// group and use the existing fresh per-turn authorization checks.
func ResolveResponsesWebSocketGroup(c *gin.Context, frame []byte) (*service.APIKey, error) {
	selection := responsesWSSelection(c)
	if selection == nil || selection.selected != nil {
		key, _ := GetAPIKeyFromContext(c)
		return key, nil
	}
	key, err := resolveAPIKeyRequestGroup(c, selection.keys, selection.subscriptions, selection.cfg, selection.original, false, frame)
	if err != nil {
		return nil, err
	}
	if key == nil || key.User == nil {
		return nil, infraerrors.Unauthorized("INVALID_API_KEY", "Invalid API key")
	}
	if code, message, ok := validateAPIKeyGroupAvailable(key); !ok {
		return nil, infraerrors.Forbidden(code, message)
	}
	if !validateAPIKeyGroupAllowed(key) {
		return nil, infraerrors.Forbidden("GROUP_NOT_ALLOWED", "Group access denied")
	}
	var subscription *service.UserSubscription
	if selection.cfg.RunMode != config.RunModeSimple && key.Group != nil && key.Group.IsSubscriptionType() && selection.subscriptions != nil {
		subscription, err = selection.subscriptions.GetActiveSubscription(c.Request.Context(), key.UserID, key.Group.ID)
		if err != nil {
			return nil, err
		}
	}
	c.Set(string(ContextKeySubscription), subscription)
	c.Set(string(ContextKeyAPIKey), key)
	setGroupContext(c, key.Group)
	SetOpsFallbackAPIKey(c, key)
	selection.selected = key
	// Replace only the group-specific revalidator. The connection's admission
	// owner and cancellation lifecycle remain owned by the auth middleware.
	revalidate := newAPIKeyAuthRevalidator(apiKeyAdmissionLookup(c, selection.keys, key), selection.original.Key,
		ip.GetSecurityClientIP(c, selection.cfg.TrustForwardedIPForAPIKeyACL()), key, true)
	ctx := service.WithUserConcurrencyPolicy(c.Request.Context(), key, revalidate)
	ctx = service.WithAPIKeyQueueAuthRevalidator(ctx, revalidate)
	c.Request = c.Request.WithContext(ctx)
	if err := bindResponsesWSAntiDistillation(c, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Keep the status private in the HTTP handshake; after upgrade the handler
// maps these ordinary authorization errors to WS policy/retry close codes.
func ResponsesWebSocketGroupErrorRetryable(err error) bool {
	return infraerrors.Code(err) >= http.StatusInternalServerError
}
