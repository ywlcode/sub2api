//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPromptCacheShadowReviewFailoverReplacesFinishedContext(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	ctx, first := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	// Regional metering and encrypted-history recovery retain this context.
	c.Request = c.Request.WithContext(ctx)
	first.finish(nil, errors.New("first account failed"))
	other := *account
	other.ID++
	nextCtx, second := svc.beginPromptCacheShadow(c.Request.Context(), c, &other)
	require.NotNil(t, second, "a completed account attempt must not suppress failover")
	capturePromptCacheShadow(nextCtx, shadowTestBody(), "http")
	second.finish(shadowTestResult(), nil)
	require.Len(t, *events, 2)
	require.Equal(t, other.ID, (*events)[1]["account_id"])
	require.Equal(t, true, (*events)[1]["committed"])
	thirdCtx, third := svc.beginPromptCacheShadow(nextCtx, c, &other)
	require.NotNil(t, third, "same-account retry also replaces a completed attempt")
	capturePromptCacheShadow(thirdCtx, shadowTestBody(), "http")
	third.finish(shadowTestResult(), nil)
	require.Equal(t, "hit", (*events)[2]["decision"])
}

func TestPromptCacheShadowReviewActiveContextIsAccountScoped(t *testing.T) {
	svc, account, c, _ := shadowTestService(t)
	ctx, first := svc.beginPromptCacheShadow(context.Background(), c, account)
	_, nested := svc.beginPromptCacheShadow(ctx, c, account)
	require.Nil(t, nested, "same active account keeps the owning outer attempt")
	other := *account
	other.ID++
	_, separate := svc.beginPromptCacheShadow(ctx, c, &other)
	require.NotNil(t, separate, "a different account needs its own attempt")
	first.finish(nil, context.Canceled)
	separate.finish(nil, context.Canceled)
}

func TestPromptCacheShadowReviewNativeCacheSnapshots(t *testing.T) {
	for _, tt := range []struct {
		name     string
		payloads []string
	}{
		{"zero delta", []string{`{"type":"message_start","message":{"usage":{"cache_read_input_tokens":1536}}}`, `{"type":"message_delta","usage":{"cache_read_input_tokens":0}}`}},
		{"null delta", []string{`{"type":"message_start","message":{"usage":{"cache_read_input_tokens":1536}}}`, `{"type":"message_delta","usage":{"cache_read_input_tokens":null}}`}},
		{"invalid delta", []string{`{"type":"message_start","message":{"usage":{"cache_read_input_tokens":1536}}}`, `{"type":"message_delta","usage":{"cache_read_input_tokens":"unknown"}}`}},
		{"canonical input detail", []string{`{"usage":{"input_tokens_details":{"cached_tokens":1536}}}`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, account, c, events := shadowTestService(t)
			ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
			capturePromptCacheShadow(ctx, shadowTestBody(), "http")
			for _, payload := range tt.payloads {
				capturePromptCacheShadowAnthropicUsage(ctx, payload)
			}
			r := shadowTestResult()
			r.Usage.CacheReadKnown = false
			before := *r
			a.finish(r, nil)
			require.Equal(t, before, *r, "diagnostics must not alter billing or protection evidence")
			require.Equal(t, true, (*events)[0]["upstream_cache_known"])
			require.Equal(t, true, (*events)[0]["comparable"])
		})
	}
}

func TestPromptCacheShadowReviewCCNativeAnthropicCacheBothModes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			svc, _, c, events := shadowTestService(t)
			account := nativeAnthropicTestAccount()
			account.Platform = PlatformOpenAI
			account.GroupIDs = []int64{2}
			cfg := rawChatCompletionsTestConfig()
			cfg.Gateway.PromptCacheShadow = svc.cfg.Gateway.PromptCacheShadow
			svc.cfg = cfg
			resp := nativeAnthropicStreamResponse()
			wire, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			wire = []byte(strings.Replace(string(wire), `"input_tokens":93,"output_tokens":1`, `"input_tokens":93,"output_tokens":1,"cache_read_input_tokens":1536`, 1))
			resp.Body = io.NopCloser(strings.NewReader(string(wire)))
			// OpenAI-only diagnostics stay restricted even though these shared
			// compatibility parsers normally serve CN-platform accounts.
			ctx, attempt := svc.beginPromptCacheShadow(context.Background(), c, account)
			capturePromptCacheShadow(ctx, []byte(`{"model":"k3","messages":[{"role":"user","content":"hello"}]}`), "http")
			resp.Request = c.Request.WithContext(ctx)
			var result *OpenAIForwardResult
			if stream {
				result, err = svc.handleCCStreamingFromNativeAnthropic(resp, c, "k3", "k3", "k3", nil, time.Now(), account)
			} else {
				result, err = svc.handleCCBufferedFromNativeAnthropic(resp, c, "k3", "k3", "k3", nil, time.Now(), account)
			}
			require.NoError(t, err)
			attempt.finish(result, err)
			require.Equal(t, 1536, result.Usage.CacheReadInputTokens)
			require.False(t, result.Usage.CacheReadKnown, "must not change existing protection behavior")
			require.Len(t, *events, 1)
			require.Equal(t, true, (*events)[0]["upstream_cache_known"])
			require.Equal(t, true, (*events)[0]["comparable"])
		})
	}
}

type shadowPanickingUpstream struct{ httpUpstreamRecorder }

func (*shadowPanickingUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	panic("observer regression upstream panic")
}

func TestPromptCacheShadowReviewCompatibilityPanicFinishes(t *testing.T) {
	for _, endpoint := range []string{"chat", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			svc, _, c, events := shadowTestService(t)
			cfg := rawChatCompletionsTestConfig()
			cfg.Gateway.PromptCacheShadow = svc.cfg.Gateway.PromptCacheShadow
			svc.cfg, svc.httpUpstream = cfg, &shadowPanickingUpstream{}
			account := forceChatResponsesFallbackAccount()
			account.GroupIDs = []int64{2}
			body := []byte(`{"model":"gpt-6.2-sol","max_tokens":32,"messages":[{"role":"user","content":"` + strings.Repeat("context ", 200) + `"}]}`)
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				if endpoint == "chat" {
					_, _ = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				} else {
					_, _ = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				}
			}()
			require.True(t, panicked, "fixture must reach the actual send boundary")
			require.Len(t, *events, 1, "panic unwinding must finish and release the observer")
			require.Equal(t, false, (*events)[0]["committed"])
		})
	}
}

func TestPromptCacheShadowReviewWebSocketAbandonedTurnFinishes(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	hooks, cleanup := svc.wrapPromptCacheShadowHooks(c, account, nil)
	defer cleanup()
	hooks.ObserveUpstreamRequest(1, shadowTestBody())
	hooks.ObserveUpstreamRequest(2, shadowTestBody())
	require.Len(t, *events, 1, "replacing a turn must finish the abandoned attempt")
	require.Equal(t, false, (*events)[0]["committed"])
	hooks.AfterTurn(1, shadowTestResult(), nil)
	require.Len(t, *events, 1, "late previous-turn completion must not finish the new turn")
	hooks.AfterTurn(2, shadowTestResult(), nil)
	require.Len(t, *events, 2)
	require.Equal(t, true, (*events)[1]["committed"])
}

func TestPromptCacheShadowReviewUnobservedFailoverMasksContext(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
	capturePromptCacheShadow(ctx, shadowTestBody(), "http")
	cn := *account
	cn.Platform = PlatformKimi
	masked, skipped := svc.beginPromptCacheShadow(ctx, c, &cn)
	require.Nil(t, skipped)
	capturePromptCacheShadow(masked, shadowTestBody(), "http")
	require.Equal(t, 1, a.captures, "an unobserved account cannot capture into a prior OpenAI attempt")
	a.finish(nil, context.Canceled)
	require.Len(t, *events, 1)
}

func TestPromptCacheShadowReviewActualOutboundHeaders(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		t.Run(transport, func(t *testing.T) {
			svc, account, c, events := shadowTestService(t)
			for _, session := range []string{"PRIVATE-session-a", "PRIVATE-session-b"} {
				ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
				headers := http.Header{}
				headers.Set("session_id", session)
				if transport == "http" {
					r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/responses", bytes.NewReader(shadowTestBody()))
					require.NoError(t, err)
					r.Header = headers
					capturePromptCacheShadowHTTP(r)
				} else {
					capturePromptCacheShadowValue(ctx, openAIWSPreparedJSON(shadowTestBody()), headers)
				}
				a.finish(shadowTestResult(), nil)
			}
			require.Len(t, *events, 2)
			require.Equal(t, "miss", (*events)[1]["decision"])
			require.Zero(t, (*events)[1]["matched_prefix_tokens"])
			require.Positive(t, (*events)[1]["shared_prefix_tokens"])
			require.NotEqual(t, (*events)[0]["routing_fingerprint"], (*events)[1]["routing_fingerprint"])
			raw, err := json.Marshal(*events)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "PRIVATE-session")
		})
	}
}

func TestPromptCacheShadowReviewMessagesWithoutSystemUsesWireProtocol(t *testing.T) {
	svc, account, c, events := shadowTestService(t)
	for _, system := range []string{"", `,"system":""`, `,"system":null`} {
		ctx, a := svc.beginPromptCacheShadow(context.Background(), c, account)
		body := []byte(`{"model":"gpt-6.2-sol","messages":[{"role":"user","content":"` + strings.Repeat("context ", 100) + `"}]` + system + `}`)
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/messages", bytes.NewReader(body))
		require.NoError(t, err)
		capturePromptCacheShadowHTTP(r)
		a.finish(shadowTestResult(), nil)
	}
	for _, event := range *events {
		require.Equal(t, "messages", event["protocol"])
	}
	require.Equal(t, "hit", (*events)[1]["decision"])
	require.Equal(t, "hit", (*events)[2]["decision"])
}

func TestPromptCacheShadowReviewBoundTicketCapturesRewrittenSession(t *testing.T) {
	observer, _, c, events := shadowTestService(t)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")),
	}}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}, upstream)
	svc.cfg.Gateway.PromptCacheShadow = observer.cfg.Gateway.PromptCacheShadow
	svc.promptCacheShadow.emit = observer.promptCacheShadow.emit
	account := ticketTestAccount(41)
	state := fakeCodexTicketState(292)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		Model: "gpt-6-astra", State: state, Length: 292, HarvestSessionID: "PRIVATE-ticket-session",
		CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	ctx, attempt := svc.beginPromptCacheShadow(context.Background(), c, account)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/responses",
		strings.NewReader(`{"model":"gpt-6-astra","input":"`+strings.Repeat("context ", 100)+`"}`))
	require.NoError(t, err)
	req.Header.Set(openAICodexTurnStateHeader, state)
	req.Header.Set("session_id", "PRIVATE-client-session")
	resp, err := svc.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	attempt.finish(shadowTestResult(), nil)
	require.Equal(t, "PRIVATE-ticket-session", upstream.lastReq.Header.Get("session_id"))
	require.Len(t, *events, 1)
	require.EqualValues(t, 1, (*events)[0]["captures"], "observe only after ticket identity rewrites")
	require.Equal(t, attempt.engine.RoutingFingerprint(upstream.lastReq.Header.Get("session_id"), upstream.lastReq.Header.Get("conversation_id")),
		(*events)[0]["routing_fingerprint"])
	raw, err := json.Marshal(*events)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE-")
}
