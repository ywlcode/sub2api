//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type cyberPassthroughRuleRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *cyberPassthroughRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

func TestResponsesWebSocketCyberPolicyBypassesConfiguredPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.failed","response":{"id":"resp_cyber_rule","model":"gpt-5.1","status":"failed","error":{"code":"cyber_policy","message":"blocked by upstream policy"},"usage":{"input_tokens":11,"output_tokens":3}}}`))
		_, _, _ = conn.Read(ctx)
	}))
	defer upstream.Close()
	message := "cyber-security 内容审计命中风险规则"
	rules := service.NewErrorPassthroughService(&cyberPassthroughRuleRepo{rules: []*model.ErrorPassthroughRule{
		{ID: 7, Enabled: true, Platforms: []string{service.PlatformOpenAI}, ErrorCodes: []int{403}, Keywords: []string{"cyber"}, MatchMode: model.MatchModeAll, PassthroughCode: true, CustomMessage: &message},
	}}, nil)
	harness := newOpenAIWSPassthroughHandlerHarnessWithSetup(t, upstream.URL, http.Header{"Session_id": {"cyber-rule-session"}}, nil, nil, func(h *OpenAIGatewayHandler) {
		h.errorPassthroughService = rules
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"test"}`)))
	_, frame, err := harness.clientConn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "response.failed", gjson.GetBytes(frame, "type").String())
	require.Equal(t, service.CyberPolicyClientCode, gjson.GetBytes(frame, "response.error.code").String())
	require.Equal(t, service.CyberPolicyClientMessage, gjson.GetBytes(frame, "response.error.message").String())
	require.NotContains(t, string(frame), message)
	require.Eventually(t, func() bool {
		logs := harness.moderationRepo.logSnapshot()
		return len(logs) == 1 && logs[0].Action == service.ContentModerationActionCyberPolicy &&
			strings.Contains(logs[0].Error, "blocked by upstream policy") &&
			strings.Contains(logs[0].Error, "upstream_usage=in:11,out:3")
	}, 3*time.Second, 10*time.Millisecond, "the handler must record original policy evidence after rewriting the client event")
}

func TestResponsesWebSocketCyberHandshakeStillRecordsAuditAndBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"cyber_policy","message":"original handshake policy"}}`))
	}))
	defer upstream.Close()
	harness := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := []byte(`{"type":"response.create","model":"gpt-5.1","input":"test"}`)
	require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, body))
	_, frame, err := harness.clientConn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, service.CyberPolicyClientCode, gjson.GetBytes(frame, "error.code").String())
	require.Equal(t, service.CyberPolicyClientMessage, gjson.GetBytes(frame, "error.message").String())
	require.NotContains(t, string(frame), "original handshake policy")
	require.Eventually(t, func() bool {
		logs := harness.moderationRepo.logSnapshot()
		return len(logs) == 1 && strings.Contains(logs[0].Error, "original handshake policy")
	}, 3*time.Second, time.Millisecond)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session_id", "ws-test-session")
	identity := service.ResolveCyberSessionIdentity(harness.apiKey.ID, c, body)
	require.NotEmpty(t, harness.handler.gatewayService.FindCyberSessionBlockedForIdentityAndBody(ctx, service.PlatformOpenAI, harness.apiKey.ID, identity, body))
	require.Empty(t, harness.handler.gatewayService.FindCyberSessionBlockedForIdentityAndBody(ctx, service.PlatformDeepseek, harness.apiKey.ID, identity, body))
}

func TestResponsesWebSocketLaterRetryCyberHandshakeKeepsCurrentSessionIdentity(t *testing.T) {
	var connections atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if connections.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"cyber_policy","message":"retry handshake policy evidence"}}`))
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
		if err = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_before_retry","model":"gpt-5.1","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`)); err != nil {
			return
		}
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
		_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}`))
		_, _, _ = conn.Read(ctx)
	}))
	defer upstream.Close()
	harness := newOpenAIWSPassthroughHandlerHarnessWithOptions(t, upstream.URL, nil, map[string]string{service.SettingKeyCyberSessionIdentityStrictEnabled: "true"}, func(a *service.Account, cfg *config.Config, _ *service.HTTPUpstream, repo *service.AccountRepository) {
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeCtxPool
		cfg.Gateway.MaxAccountSwitches = 1
		a.Priority = 1
		other := *a
		other.ID++
		other.Priority = 2
		*repo = &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{*a, other}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := []byte(`{"type":"response.create","model":"gpt-5.1","response":{"client_metadata":{"thread_id":"retry-bound-thread"},"input":"first"}}`)
	second := []byte(`{"type":"response.create","model":"gpt-5.1","response":{"input":"second turn needs retry"}}`)
	require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, first))
	_, frame, err := harness.clientConn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "resp_before_retry", gjson.GetBytes(frame, "response.id").String())
	require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, second))
	_, frame, err = harness.clientConn.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(frame), service.CyberPolicyClientMessage)
	require.NotContains(t, string(frame), "retry handshake policy evidence")
	require.GreaterOrEqual(t, connections.Load(), int32(2))
	require.Eventually(t, func() bool { return len(harness.moderationRepo.logSnapshot()) == 1 }, 3*time.Second, 10*time.Millisecond)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	identity := service.ResolveCyberSessionIdentity(harness.apiKey.ID, c, first)
	require.NotEmpty(t, harness.handler.gatewayService.FindCyberSessionBlockedForIdentityAndBody(ctx, service.PlatformOpenAI, harness.apiKey.ID, identity, second))
	require.Empty(t, harness.handler.gatewayService.FindCyberSessionBlockedForIdentityAndBody(ctx, service.PlatformDeepseek, harness.apiKey.ID, identity, second))
}
