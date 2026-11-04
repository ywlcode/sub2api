//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIHTTPErrorPassthroughUsesAccountPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const overload = "Our servers are currently overloaded. Please try again later."
	platforms := domain.PlatformIDsWhere(func(spec domain.PlatformSpec) bool {
		return spec.Gateway == domain.PlatformGatewayOpenAI
	})
	for _, platform := range platforms {
		for _, scope := range []string{"openai_only", "account_platform", "all_platforms"} {
			for _, upstreamStatus := range []int{http.StatusInternalServerError, http.StatusUnprocessableEntity} {
				t.Run(fmt.Sprintf("%s/%s/status=%d", platform, scope, upstreamStatus), func(t *testing.T) {
					code := http.StatusBadGateway
					openAIMessage := overload
					rules := []*model.ErrorPassthroughRule{{
						ID: 1, Enabled: true, Platforms: []string{PlatformOpenAI},
						ErrorCodes: []int{upstreamStatus}, MatchMode: model.MatchModeAll,
						ResponseCode: &code, CustomMessage: &openAIMessage, SkipMonitoring: true,
					}}
					expected := "Upstream request failed"
					if scope != "openai_only" {
						var rulePlatforms []string
						expected = "Global upstream error"
						if scope == "account_platform" {
							rulePlatforms = []string{platform}
							expected = "Provider-specific error"
						}
						message := expected
						rules = append(rules, &model.ErrorPassthroughRule{
							ID: 2, Enabled: true, Priority: 1, Platforms: rulePlatforms,
							ErrorCodes: []int{upstreamStatus}, MatchMode: model.MatchModeAll,
							ResponseCode: &code, CustomMessage: &message,
						})
					}
					if platform == PlatformOpenAI {
						expected = overload
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					ruleSvc := &ErrorPassthroughService{}
					ruleSvc.setLocalCache(rules)
					BindErrorPassthroughService(c, ruleSvc)
					resp := &http.Response{
						StatusCode: upstreamStatus, Header: http.Header{},
						Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Upstream gateway error","type":"upstream_error"}}`)),
					}
					account := &Account{ID: 1, Platform: platform, Type: AccountTypeAPIKey}
					_, err := (&OpenAIGatewayService{}).handleErrorResponse(context.Background(), resp, c, account, nil)

					require.Error(t, err)
					require.Equal(t, http.StatusBadGateway, rec.Code)
					require.Equal(t, expected, gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
					require.Equal(t, platform == PlatformOpenAI, c.GetBool(OpsSkipPassthroughKey))
					require.Equal(t, upstreamStatus, c.GetInt(OpsUpstreamStatusCodeKey))
					require.Equal(t, "Upstream gateway error", c.GetString(OpsUpstreamErrorMessageKey))
				})
			}
		}
	}
}
