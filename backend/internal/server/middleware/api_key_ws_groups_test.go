//go:build unit

package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type wsModelGroupProbe struct{ calls []string }

func (p *wsModelGroupProbe) ProbeAPIKeyGroup(_ context.Context, group *service.Group, model, _ string) (service.APIKeyGroupAvailability, error) {
	p.calls = append(p.calls, model)
	return service.APIKeyGroupAvailability{Available: group.ID == 2 && model == "gpt-second"}, nil
}

func TestResponsesWSMultiGroupSelectsFromFirstFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, query := range []string{"", "?model=wrong-query-model"} {
			t.Run(path+query, func(t *testing.T) {
				primary := &service.Group{ID: 1, Platform: service.PlatformComposite, Status: service.StatusActive, Hydrated: true, SubscriptionType: service.SubscriptionTypeStandard}
				second := &service.Group{ID: 2, Platform: service.PlatformComposite, Status: service.StatusActive, Hydrated: true, SubscriptionType: service.SubscriptionTypeStandard, BlockSubagents: true}
				key := &service.APIKey{ID: 10, UserID: 7, Key: "ws-fixture-key", Status: service.StatusActive,
					GroupID: &primary.ID, GroupIDs: []int64{1, 2}, Group: primary,
					User: &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 100}}
				cfg := &config.Config{RunMode: config.RunModeStandard}
				revoked := false
				repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
					copyKey := *key
					if revoked {
						copyKey.Status = service.StatusAPIKeyDisabled
					}
					return &copyKey, nil
				}}
				keys := service.NewAPIKeyService(repo, nil, modelListGroupRepo{groups: map[int64]*service.Group{1: primary, 2: second}}, nil, nil, nil, cfg)
				probe := &wsModelGroupProbe{}
				keys.SetGroupRouter(probe)
				router := gin.New()
				router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)), GroupCodexCLIOnly(nil), GroupBlockSubagents())
				done := make(chan map[string]any, 1)
				router.GET(path, func(c *gin.Context) {
					result := map[string]any{"handshake_probes": len(probe.calls), "pending": ResponsesWebSocketGroupPending(c)}
					defer func() { done <- result }()
					conn, err := websocket.Accept(c.Writer, c.Request, nil)
					if err != nil {
						result["error"] = err.Error()
						return
					}
					defer conn.CloseNow()
					_, frame, err := conn.Read(c.Request.Context())
					if err != nil {
						result["error"] = err.Error()
						return
					}
					selected, err := ResolveResponsesWebSocketGroup(c, frame)
					if err != nil {
						result["error"] = err.Error()
						return
					}
					result["group"] = *selected.GroupID
					result["pending_after"] = ResponsesWebSocketGroupPending(c)
					_, installed, err := (&service.ConcurrencyService{}).RevalidateAPIKeyQueueTurn(c.Request.Context())
					result["revalidator"] = installed
					if err != nil {
						result["error"] = err.Error()
						return
					}
					result["main_allowed"] = !CheckGroupSubagentFrame(c, frame)
					result["subagent_blocked"] = CheckGroupSubagentFrame(c, []byte(`{"type":"response.create","model":"gpt-second","client_metadata":{"x-openai-subagent":"worker"}}`))
					revoked = true
					_, _, revokedErr := (&service.ConcurrencyService{}).RevalidateAPIKeyQueueTurn(c.Request.Context())
					result["revocation_rejected"] = revokedErr != nil
					data, _ := json.Marshal(result)
					_ = conn.Write(c.Request.Context(), websocket.MessageText, data)
				})
				server := httptest.NewServer(router)
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+path+query, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key.Key}}})
				require.NoError(t, err)
				defer conn.CloseNow()
				require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
				require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-second","input":"hello"}`)))
				_, _, err = conn.Read(ctx)
				result := <-done
				require.NoError(t, err, "%v", result)
				require.NotContains(t, result, "error")
				require.Equal(t, 0, result["handshake_probes"])
				require.Equal(t, true, result["pending"])
				require.Equal(t, int64(2), result["group"])
				require.Equal(t, false, result["pending_after"])
				require.Equal(t, true, result["revalidator"])
				require.Equal(t, true, result["main_allowed"])
				require.Equal(t, true, result["subagent_blocked"])
				require.Equal(t, true, result["revocation_rejected"])
				require.Equal(t, int64(1), *key.GroupID, "shared authentication snapshot must remain unchanged")
				require.NotContains(t, probe.calls, "")
				require.NotContains(t, probe.calls, "wrong-query-model")
			})
		}
	}
}
