//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCompositeChannelMappingHTTPSelectsTargetAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const publicModel, targetModel = "gpt-5.6-luna", "gpt-6.1-sol"
	for _, endpoint := range []string{"responses", "responses-stream", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			groupID := int64(3132)
			account := service.Account{
				ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID},
				Credentials: map[string]any{
					"api_key": "sk-test", "model_mapping": map[string]any{targetModel: targetModel},
				},
			}
			channels := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
				channels: []service.Channel{{
					ID: 1, Status: service.StatusActive, GroupIDs: []int64{groupID},
					ModelMapping: map[string]map[string]string{service.PlatformOpenAI: {publicModel: targetModel}},
				}},
				groupPlatforms: map[int64]string{groupID: service.PlatformComposite},
			}, nil, nil, nil, nil)
			upstream := newAstraProCapturedUpstream(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-6.1-sol","status":"completed",` +
					`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
					`"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n")),
			})
			cfg := &config.Config{RunMode: config.RunModeSimple}
			gateway := service.NewOpenAIGatewayService(
				&openAIWSUsageHandlerAccountRepoStub{account: account}, nil, nil, nil, nil, nil, nil, nil,
				cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, channels, nil, nil, nil,
			)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			body := `{"model":"gpt-5.6-luna","stream":false,"input":"hello","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
			if endpoint == "responses-stream" {
				body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
			}
			c, rec := newAstraProFailoverContext(t, body)
			c.Request.URL.Path = "/v1/" + strings.TrimSuffix(endpoint, "-stream")
			apiKey, _ := middleware2.GetAPIKeyFromContext(c)
			apiKey.Group.Platform = service.PlatformComposite
			apiKey.Group.AllowMessagesDispatch = true
			// An unrelated group dispatch fallback must not override the channel rewrite.
			apiKey.Group.MessagesDispatchModelConfig.ExactModelMappings = map[string]string{publicModel: "gpt-5.1"}
			c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
				Matched: true, Source: service.CompositeRouteSourceAccount, GroupID: groupID,
				PublicModel: publicModel, TargetPlatform: service.PlatformOpenAI, UpstreamModel: publicModel,
			}))
			switch endpoint {
			case "responses", "responses-stream":
				h.Responses(c)
			case "chat/completions":
				h.ChatCompletions(c)
			case "messages":
				h.Messages(c)
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			_, ids, bodies := upstream.snapshot()
			require.Equal(t, []int64{account.ID}, ids)
			require.Equal(t, targetModel, gjson.GetBytes(bodies[0], "model").String())
			original, ok := service.RequestedPublicModelFromContext(c.Request.Context())
			require.True(t, ok)
			require.Equal(t, publicModel, original)
		})
	}
}
