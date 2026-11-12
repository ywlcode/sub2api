//go:build integration

package repository_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type siteTestUpstream struct {
	client *http.Client
	host   string
}

func (u siteTestUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if r.URL.Host != u.host {
		return nil, fmt.Errorf("unexpected test upstream")
	}
	return u.client.Do(r)
}
func (u siteTestUpstream) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, id, n)
}

func TestSiteSecondaryGatewayUsesLocalAuthSchedulerProtocolAndBilling(t *testing.T) {
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	primaryDB, primaryClient := repository.SiteTestDatabase(t)
	localDB, localClient := repository.SiteTestDatabase(t)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if strings.HasSuffix(r.URL.Path, "/messages") {
			require.Equal(t, "domestic-secret", r.Header.Get("x-api-key"))
		} else {
			require.Equal(t, "Bearer domestic-secret", r.Header.Get("Authorization"))
		}
		require.Equal(t, "deepseek-chat", gjson.GetBytes(raw, "model").String())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses"):
			require.Equal(t, "high", gjson.GetBytes(raw, "reasoning.effort").String())
			_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","model":"deepseek-chat","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":20}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = io.WriteString(w, `{"id":"chat_test","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","model":"deepseek-chat","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	group := repository.SiteTestGroup(t, primaryClient, &service.Group{Name: "domestic-" + uuid.NewString(), Platform: service.PlatformDeepseek, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1, Status: service.StatusActive})
	user := repository.SiteTestUser(t, primaryClient, &service.User{Email: uuid.NewString() + "@example.invalid", PasswordHash: "disabled", Balance: 100, Concurrency: 10, Status: service.StatusActive})
	key := repository.SiteTestKey(t, primaryClient, &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-client-" + uuid.NewString(), Name: "client", Status: service.StatusActive, Quota: 100})
	account := repository.SiteTestAccount(t, primaryClient, &service.Account{Name: "domestic-" + uuid.NewString(), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 10, Extra: map[string]any{service.AccountExtraSecondaryEnabled: true},
		Credentials: map[string]any{"api_key": "domestic-secret", "model_mapping": map[string]any{"deepseek-chat": "deepseek-chat"}, "base_url": upstream.URL, service.DeepSeekVolcengineCompatibilityCredential: true, "api_protocol": service.APIProtocolResponses, "api_base_urls": map[string]any{service.APIProtocolResponses: upstream.URL, service.APIProtocolChatCompletions: upstream.URL, service.APIProtocolAnthropic: upstream.URL}}})
	_, err := primaryDB.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account.ID, group.ID)
	require.NoError(t, err)
	peer := site.Peer{ID: "office", Token: strings.Repeat("t", 32)}
	primary, err := repository.NewSiteStore(primaryDB, &config.Config{Site: site.Config{Mode: site.Primary, ID: "bosc"}})
	require.NoError(t, err)
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token}}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxBodySize = 1 << 20
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = time.Second
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	local, err := repository.NewSiteStore(localDB, cfg)
	require.NoError(t, err)
	snapshot, err := primary.Export(ctx, peer, nil, nil)
	require.NoError(t, err)
	require.NoError(t, local.Install(ctx, site.SyncResponse{PrimaryID: "bosc", Full: true, Version: "1", Rows: snapshot.Rows}))
	primaryManager, err := site.New(site.Config{Mode: site.Primary, ID: "bosc", Peers: []site.Peer{peer}}, primary, 1<<20)
	require.NoError(t, err)
	control := gin.New()
	control.POST(site.SyncPath, primaryManager.SyncHandler)
	controlServer := httptest.NewServer(control)
	defer controlServer.Close()
	cfg.Site.PrimaryURL, cfg.Site.AllowHTTP = controlServer.URL, true
	localManager, err := site.New(cfg.Site, local, 1<<20)
	require.NoError(t, err)
	localManager.Start()
	defer localManager.Stop()
	require.Eventually(t, localManager.Ready, 3*time.Second, 10*time.Millisecond)
	users := repository.NewUserRepository(localClient, localDB)
	groups := repository.NewGroupRepository(localClient, localDB)
	keys := repository.NewAPIKeyRepository(localClient, localDB)
	accounts := repository.NewAccountRepository(localClient, localDB, nil)
	slots := service.NewConcurrencyService(repository.NewConcurrencyCache(repository.Issue594TestRedis(), 1, 60))
	billing := service.NewBillingCacheService(nil, users, nil, keys, nil, nil, cfg, nil)
	defer billing.Stop()
	scheduler := service.NewSchedulerSnapshotService(nil, nil, accounts, groups, cfg)
	deferred := service.NewDeferredService(accounts, nil, time.Hour)
	prices := service.NewBillingService(cfg, nil)
	gateway := service.NewOpenAIGatewayService(accounts, nil, repository.NewUsageLogRepository(localClient, localDB), repository.ProvideUsageBillingRepository(localClient, localDB, cfg), users, nil, nil, repository.NewGatewayCache(repository.Issue594TestRedis()), cfg, scheduler, slots, prices, nil, billing, siteTestUpstream{client: upstream.Client(), host: strings.TrimPrefix(upstream.URL, "http://")}, deferred, nil, nil, nil, nil, nil, nil, nil)
	defer gateway.StopOpenAICodexTicketHarvester()
	defer gateway.CloseOpenAIWSPool()
	keyService := service.NewAPIKeyService(keys, users, groups, nil, nil, nil, cfg)
	h := handler.NewOpenAIGatewayHandler(gateway, slots, billing, keyService, nil, nil, nil, nil, cfg)
	r := gin.New()
	r.Use(middleware.RequestLogger(), middleware.RequestTiming(local), localManager.Ingress())
	r.Use(gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(keyService, nil, cfg)))
	r.POST("/v1/responses", h.Responses)
	r.POST("/v1/chat/completions", h.ChatCompletions)
	r.POST("/v1/messages", h.Messages)
	for _, req := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"deepseek-chat","input":"hello","reasoning":{"effort":"high"},"stream":false}`},
		{"/v1/chat/completions", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}],"stream":false}`},
		{"/v1/messages", `{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"stream":false}`},
	} {
		request := httptest.NewRequest("POST", req.path, strings.NewReader(req.body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+key.Key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, request)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "ok")
	}
	require.EqualValues(t, 3, upstreamCalls.Load())
	localManager.Stop()
	var count int
	require.NoError(t, localDB.QueryRow(`SELECT count(*) FROM request_connection_traces WHERE (detail->>'body_bytes')::int>0 AND detail->>'body_complete'='true'`).Scan(&count))
	require.Equal(t, 3, count, "routing must not consume the body before upload timing; traces must persist before the handlers return")
	require.NoError(t, localDB.QueryRow(`SELECT count(*) FROM usage_logs`).Scan(&count))
	require.Equal(t, 3, count)
	var spent float64
	require.NoError(t, localDB.QueryRow(`SELECT sum(actual_cost) FROM usage_logs`).Scan(&spent))
	require.Greater(t, spent, 0.0)
	pending, err := local.Pending(ctx, 128)
	require.NoError(t, err)
	ids, err := local.PendingIDs(ctx)
	require.NoError(t, err)
	_, err = primary.Export(ctx, peer, pending, ids)
	require.NoError(t, err)
	require.NoError(t, primaryDB.QueryRow(`SELECT count(*) FROM request_timing_details d JOIN usage_logs u ON u.id=d.usage_log_id WHERE u.request_id LIKE 'site:%' AND d.detail->>'body_complete'='true'`).Scan(&count))
	require.Equal(t, 3, count, "primary usage timing statistics must include secondary requests")
	var localBalance, primaryBalance float64
	require.NoError(t, localDB.QueryRow(`SELECT balance FROM users WHERE id=$1`, user.ID).Scan(&localBalance))
	require.NoError(t, primaryDB.QueryRow(`SELECT balance FROM users WHERE id=$1`, user.ID).Scan(&primaryBalance))
	require.Equal(t, localBalance, primaryBalance)
}
