//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/model"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type platformPassthroughRuleRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *platformPassthroughRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

func platformPassthroughRule(platform, message string, priority int) *model.ErrorPassthroughRule {
	code := http.StatusBadGateway
	var platforms []string
	if platform != "" {
		platforms = []string{platform}
	}
	return &model.ErrorPassthroughRule{
		ID: int64(priority + 1), Enabled: true, Priority: priority,
		Platforms: platforms, ErrorCodes: []int{500, 502, 503, 504, 529},
		MatchMode: model.MatchModeAll, ResponseCode: &code, CustomMessage: &message,
		SkipMonitoring: platform == service.PlatformOpenAI,
	}
}

func TestOpenAIFailoverPassthroughUsesAccountPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const overload = "Our servers are currently overloaded. Please try again later."
	platforms := domain.PlatformIDsWhere(func(spec domain.PlatformSpec) bool {
		return spec.Gateway == domain.PlatformGatewayOpenAI
	})
	for _, platform := range platforms {
		for _, ruleScope := range []string{"openai_only", "account_platform", "all_platforms"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", platform, ruleScope, streaming), func(t *testing.T) {
					rules := []*model.ErrorPassthroughRule{platformPassthroughRule(service.PlatformOpenAI, overload, 0)}
					expected := "Upstream service temporarily unavailable"
					switch ruleScope {
					case "account_platform":
						expected = "Provider-specific error"
						rules = append(rules, platformPassthroughRule(platform, expected, 1))
					case "all_platforms":
						expected = "Global upstream error"
						rules = append(rules, platformPassthroughRule("", expected, 1))
					}
					if platform == service.PlatformOpenAI {
						expected = overload
					}
					h := &OpenAIGatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(&platformPassthroughRuleRepo{rules: rules}, nil)}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
					c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}})
					// A selected account must win over the route's original target and
					// the previous attempt, including after a failed account switch.
					c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenAI))
					setOpsSelectedAccount(c, 11, service.PlatformOpenAI)
					setOpsSelectedAccount(c, 12, platform)
					if streaming {
						c.Writer.Header().Set("Content-Type", "text/event-stream")
						c.Writer.WriteHeaderNow()
					}

					h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
						StatusCode:   http.StatusInternalServerError,
						ResponseBody: []byte(`{"error":{"message":"Upstream gateway error","type":"upstream_error"}}`),
					}, streaming)

					if streaming {
						require.Equal(t, http.StatusOK, rec.Code)
						require.Contains(t, rec.Body.String(), "event: response.failed")
						require.Contains(t, rec.Body.String(), expected)
					} else {
						require.Equal(t, http.StatusBadGateway, rec.Code)
						require.Equal(t, expected, gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
					}
					require.Equal(t, platform == service.PlatformOpenAI, c.GetBool(service.OpsSkipPassthroughKey), "a rule for another platform must not hide monitoring")
					if platform != service.PlatformOpenAI {
						require.NotContains(t, rec.Body.String(), overload)
					}
				})
			}
		}
	}
}

func TestOpenAIFailoverPassthroughWithoutSelectedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, groupPlatform, resolvedPlatform string
		noRequest, wantOpenAI                 bool
	}{
		{name: "composite_deepseek", groupPlatform: service.PlatformComposite, resolvedPlatform: service.PlatformDeepseek},
		{name: "direct_kimi", groupPlatform: service.PlatformKimi},
		{name: "unresolved_composite", groupPlatform: service.PlatformComposite},
		{name: "unknown_platform"},
		{name: "no_request", noRequest: true},
		{name: "direct_openai", groupPlatform: service.PlatformOpenAI, wantOpenAI: true},
		{name: "composite_openai", groupPlatform: service.PlatformComposite, resolvedPlatform: service.PlatformOpenAI, wantOpenAI: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rules := []*model.ErrorPassthroughRule{platformPassthroughRule(service.PlatformOpenAI, "OpenAI-only error", 0)}
			h := &OpenAIGatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(&platformPassthroughRuleRepo{rules: rules}, nil)}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			if !tt.noRequest {
				c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
				if tt.resolvedPlatform != "" {
					c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), tt.resolvedPlatform))
				}
			}
			if tt.groupPlatform != "" {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: tt.groupPlatform}})
			}
			h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
				StatusCode: http.StatusInternalServerError, ResponseBody: []byte(`{"error":{"message":"Upstream gateway error"}}`),
			}, false)
			expected := "Upstream service temporarily unavailable"
			if tt.wantOpenAI {
				expected = "OpenAI-only error"
			}
			require.Equal(t, expected, gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
		})
	}
}
