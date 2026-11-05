//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForbiddenFailoverRulesBeforeSpecialMappings(t *testing.T) {
	message := "configured forbidden message"
	rules := service.NewErrorPassthroughService(&platformPassthroughRuleRepo{rules: []*model.ErrorPassthroughRule{{
		ID: 1, Enabled: true, Platforms: []string{service.PlatformOpenAI}, ErrorCodes: []int{403},
		MatchMode: model.MatchModeAll, PassthroughCode: true, CustomMessage: &message, SkipMonitoring: true,
	}}}, nil)
	for _, reason := range []service.GatewayFailureReason{"", service.OpenAIUpstreamAccessStateReason, service.OpenAIImagesInsufficientBalanceReason, service.ExcelBPSRateLimitedReason} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", reason, stream), func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
				setOpsSelectedAccount(c, 144, service.PlatformOpenAI)
				if stream {
					c.Header("Content-Type", "text/event-stream")
					c.Writer.WriteHeaderNow()
				}
				err := &service.UpstreamFailoverError{StatusCode: 403, Reason: reason, ClientStatusCode: 502, ClientMessage: "fixed fallback", ResponseBody: []byte(`{"error":{"type":"permission_error","message":"original denial"}}`)}
				if reason == service.OpenAIUpstreamAccessStateReason {
					err.Stage = service.GatewayFailureStageAccountAuth
				}
				h := &OpenAIGatewayHandler{errorPassthroughService: rules}
				h.handleFailoverExhausted(c, err, stream)
				if stream {
					require.Equal(t, 200, rec.Code)
					require.Contains(t, rec.Body.String(), "event: response.failed")
					require.Contains(t, rec.Body.String(), message)
				} else {
					require.Equal(t, 403, rec.Code)
					require.Equal(t, message, gjson.Get(rec.Body.String(), "error.message").String())
				}
				require.True(t, c.GetBool(service.OpsSkipPassthroughKey))
			})
		}
	}
}

func TestForbiddenCompatFailoverRules(t *testing.T) {
	message := "configured forbidden message"
	rules := service.NewErrorPassthroughService(&platformPassthroughRuleRepo{rules: []*model.ErrorPassthroughRule{{
		ID: 1, Enabled: true, Platforms: []string{service.PlatformOpenAI}, ErrorCodes: []int{403},
		MatchMode: model.MatchModeAll, PassthroughCode: true, CustomMessage: &message,
	}}}, nil)
	for _, path := range []string{"responses", "chat", "messages", "websocket"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+path, nil)
			setOpsSelectedAccount(c, 144, service.PlatformOpenAI)
			err := &service.UpstreamFailoverError{StatusCode: 403, Stage: service.GatewayFailureStageAccountAuth, ResponseBody: []byte(`{"error":{"message":"original denial"}}`)}
			switch path {
			case "responses":
				(&GatewayHandler{errorPassthroughService: rules}).handleResponsesFailoverExhausted(c, err, false)
			case "chat":
				(&GatewayHandler{errorPassthroughService: rules}).handleCCFailoverExhausted(c, err, false)
			case "messages":
				(&OpenAIGatewayHandler{errorPassthroughService: rules}).handleAnthropicFailoverExhausted(c, err, false)
			case "websocket":
				(&OpenAIGatewayHandler{errorPassthroughService: rules}).closeOpenAIWSFailoverExhausted(c, nil, err)
				streamErr, ok := service.GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, 403, streamErr.IntendedStatus)
				require.Equal(t, message, streamErr.Message)
				return
			}
			require.Equal(t, 403, rec.Code)
			require.Equal(t, message, gjson.Get(rec.Body.String(), "error.message").String())
		})
	}
}
