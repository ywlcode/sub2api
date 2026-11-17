package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const history200Success = `{"type":"response.completed","response":{"id":"accepted-attempt","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued"}]}],"usage":{"input_tokens":5,"output_tokens":1}}}`
const history200FunctionFailure = `{"type":"response.failed","response":{"id":"rejected-attempt","status":"failed","error":{"code":"server_error","message":"OpenAI Responses: Encrypted function output content could not be decrypted or decoded."}}}`

func enableHistory200(s *OpenAIGatewayService, enabled, enabled400 bool) {
	s.settingService = NewSettingService(&history400SettingsRepo{values: map[string]string{
		SettingKeyOpenAI200OmitEncryptedHistoryEnabled: fmt.Sprint(enabled),
		SettingKeyOpenAI400OmitEncryptedHistoryEnabled: fmt.Sprint(enabled400),
	}}, s.cfg)
}

func TestOpenAI200HistoryNativeForward(t *testing.T) {
	const located = `{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content","message":"The encrypted content AAAA...1111 could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}}`
	failure := encryptedSSERetryEvent(encryptedSSERetryFailure)
	preamble := encryptedSSERetryEvent(`{"type":"response.created","response":{"id":"rejected-attempt","status":"in_progress","padding":"` + strings.Repeat("p", 20000) + `"}}`)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, tc := range []struct {
			name, first, second, input            string
			firstHTTP, secondHTTP                 int
			enabled, enabled400, after409, optOut bool
			calls                                 int
			success, lossy                        bool
		}{
			{name: "off", calls: 1},
			{name: "400 alone cannot recover SSE", enabled400: true, calls: 1},
			{name: "enabled", enabled: true, calls: 2, success: true, lossy: true},
			{name: "owner binding opt out", enabled: true, optOut: true, calls: 2, success: true, lossy: true},
			{name: "after409", enabled: true, after409: true, calls: 2, success: true, lossy: true},
			{name: "created then failed", first: preamble + failure, enabled: true, calls: 2, success: true, lossy: true},
			{name: "bare nested error", first: encryptedSSERetryEvent(`{"type":"error","error":{"code":"invalid_encrypted_content","message":"rejected"}}`), enabled: true, calls: 2, success: true, lossy: true},
			{name: "bare flat error", first: encryptedSSERetryEvent(`{"type":"error","code":"invalid_encrypted_content","message":"rejected"}`), enabled: true, calls: 2, success: true, lossy: true},
			{name: "named multiline event", first: "event: response.failed\ndata: {\"response\": {\"error\":\ndata: {\"code\":\"invalid_encrypted_content\",\"message\":\"rejected\"}}}\n\n", enabled: true, calls: 2, success: true, lossy: true},
			{name: "encrypted function output phrase", first: encryptedSSERetryEvent(history200FunctionFailure), enabled: true, calls: 2, success: true, lossy: true},
			{name: "phrase and old encrypted tool result", first: encryptedSSERetryEvent(history200FunctionFailure), input: `[{"type":"function_call","call_id":"call_old","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_old","output":[{"type":"input_text","text":"preserve result"},{"type":"encrypted_content","encrypted_content":"opaque-old-result"}]},{"role":"user","content":"Explain the result"}]`, enabled: true, calls: 2, success: true, lossy: true},
			{name: "unrelated error", first: encryptedSSERetryEvent(`{"type":"response.failed","response":{"error":{"code":"invalid_request_error","message":"invalid tool argument"}}}`), enabled: true, calls: 1},
			{name: "redacted message is not evidence", first: encryptedSSERetryEvent(`{"type":"response.failed","response":{"error":{"code":"invalid_request_error","message":"Upstream encrypted content error (details redacted)"}}}`), enabled: true, calls: 1},
			{name: "text already delivered", first: streamLineageDeltaEvent + failure, enabled: true, calls: 1},
			{name: "reasoning already delivered", first: encryptedSSERetryEvent(`{"type":"response.reasoning_summary_text.delta","delta":"visible thought"}`) + failure, enabled: true, calls: 1},
			{name: "tool already delivered", first: encryptedSSERetryEvent(`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_live","name":"execute","arguments":"{}"}}`) + failure, enabled: true, calls: 1},
			{name: "second SSE rejection", enabled: true, second: failure, calls: 2, lossy: true},
			{name: "second phrase rejection does not rotate", enabled: true, second: encryptedSSERetryEvent(history200FunctionFailure), calls: 2, lossy: true},
			{name: "precise then lossy SSE", enabled: true, first: encryptedSSERetryEvent(located), second: failure, calls: 3, success: true, lossy: true},
			{name: "HTTP precise then SSE lossy", enabled: true, first: `{"error":{"code":"invalid_encrypted_content","message":"The encrypted content AAAA...1111 could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`, firstHTTP: 400, second: failure, calls: 3, success: true, lossy: true},
			{name: "200 switch does not enable 400", enabled: true, first: history400Failure, firstHTTP: 400, calls: 1},
			{name: "200 to 400 shares lossy budget", enabled: true, enabled400: true, second: history400Failure, secondHTTP: 400, calls: 2, lossy: true},
			{name: "400 to 200 shares lossy budget", enabled: true, enabled400: true, first: history400Failure, firstHTTP: 400, second: failure, calls: 2},
			{name: "current encrypted task", enabled: true, input: `[{"type":"reasoning","encrypted_content":"old"},{"type":"agent_message","content":"task header","encrypted_content":"current"}]`, calls: 1},
			{name: "compaction", enabled: true, input: `[{"type":"compaction","encrypted_content":"history"},{"role":"user","content":"continue"}]`, calls: 1},
			{name: "no old ciphertext", enabled: true, input: `[{"role":"user","content":"task"}]`, calls: 1},
		} {
			t.Run(accountType+"/"+tc.name, func(t *testing.T) {
				input := tc.input
				if input == "" {
					input = history400Input
				}
				body := history400Body(input, true)
				canonical := append([]byte(nil), body...)
				first := tc.first
				if first == "" {
					first = failure
				}
				second := tc.second
				if second == "" {
					second = encryptedSSERetryEvent(history200Success)
				}
				response := func(data, state string, status int) *http.Response {
					resp := newStreamLineageSSEResponse(data)
					resp.Header.Set("X-Codex-Turn-State", state)
					resp.Header.Set("X-Request-Id", state)
					if status != 0 {
						resp.StatusCode = status
						resp.Header.Set("Content-Type", "application/json")
					}
					return resp
				}
				firstResp := response(first, "rejected-state", tc.firstHTTP)
				tracked := &encryptedSSERetryBody{Reader: firstResp.Body}
				firstResp.Body = tracked
				upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{
					firstResp, response(second, "accepted-state", tc.secondHTTP), response(encryptedSSERetryEvent(history200Success), "accepted-state", 0),
				}}
				svc := newStreamLineageHTTPService(upstream)
				enableHistory200(svc, tc.enabled, tc.enabled400)
				svc.cfg.Gateway.DisableEncryptedAgentOwnerBinding = tc.optOut
				account := newStreamLineageHTTPAccount()
				account.Type = accountType
				account.Credentials["access_token"] = "synthetic-access-token"
				c, rec := newStreamLineageTestContext("history200-fixture")
				c.Set("api_key", &APIKey{ID: 7, AllowLongContext: true})
				ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, body, "history200-fixture", "gpt-5.1")
				if tc.after409 {
					r := openAIAgentRecoveryFromContext(ctx)
					r.omittedHistory, r.inputDigest = true, openAIAgentRecoveryInputDigest(body)
					r.replayControlsDigest = openAI409ControlsDigest(body)
				}
				c.Request = c.Request.WithContext(ctx)
				_, err := svc.Forward(ctx, c, account, body)
				require.Equal(t, tc.calls, upstream.callCount, "error=%v; response=%s; decision=%+v", err, rec.Body.String(), openAI200HistoryDiagnostic(c))
				require.Equal(t, canonical, body, "client source is immutable")
				if tc.success {
					require.NoError(t, err)
					require.Contains(t, rec.Body.String(), "continued")
					require.NotContains(t, rec.Body.String(), "rejected-attempt")
					require.NotContains(t, rec.Body.String(), "invalid_encrypted_content")
					require.Equal(t, "accepted-state", rec.Header().Get("X-Request-Id"))
					require.Equal(t, "accepted-state", rec.Header().Get("X-Codex-Turn-State"))
				} else {
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover, "encryption failure must not rotate accounts")
				}
				if tc.calls > 1 {
					require.True(t, tracked.closed)
				}
				if tc.lossy {
					d := openAI200HistoryDiagnostic(c)
					require.NotNil(t, d)
					require.True(t, d.Applied)
					require.Equal(t, tc.after409, d.After409)
					last := upstream.bodies[len(upstream.bodies)-1]
					require.NotContains(t, string(last), "AAAA-fixture-1111")
					require.NotContains(t, string(last), "BBBB-fixture-2222")
					if tc.input == "" {
						for _, path := range []string{"input.2", "input.3", "input.4"} {
							require.JSONEq(t, gjson.GetBytes(upstream.bodies[0], path).Raw, gjson.GetBytes(last, path).Raw, path)
						}
					} else {
						require.Equal(t, gjson.GetBytes(last, "input.0.call_id").String(), gjson.GetBytes(last, "input.1.call_id").String())
						require.Contains(t, string(last), "preserve result")
						require.NotContains(t, string(last), "opaque-old-result")
					}
					for _, req := range upstream.reqs {
						require.Equal(t, upstream.reqs[0].Header.Get("Authorization"), req.Header.Get("Authorization"))
						require.Equal(t, upstream.reqs[0].URL.String(), req.URL.String())
					}
					events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
					found := false
					for _, ev := range events {
						if ev.Reason == "encrypted_history_200_omitted" {
							found = true
							require.Equal(t, 200, ev.UpstreamStatusCode, "do not fabricate HTTP 400 for an SSE error")
							require.NotNil(t, ev.EncryptedHistory200)
							require.Equal(t, "same_account_retry_scheduled", ev.EncryptedHistory200.Decision)
							require.NotContains(t, ev.Message, "fixture")
						}
					}
					require.True(t, found)
				}
				require.NotContains(t, rec.Body.String(), "encrypted_history_200")
			})
		}
	}
}

func TestOpenAI200RejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		name, payload, event string
		matched              bool
	}{
		{"standard", encryptedSSERetryFailure, "response.failed", true},
		{"function result", history200FunctionFailure, "response.failed", true},
		{"flat phrase", `{"type":"error","message":"Encrypted function output content could not be decrypted or decoded."}`, "error", true},
		{"code missing", `{"type":"error","message":"The encrypted content could not be verified."}`, "error", false},
		{"partial phrase", `{"type":"error","message":"Encrypted function output content"}`, "error", false},
		{"message in metadata", `{"type":"error","message":"unrelated","metadata":{"message":"Encrypted function output content could not be decrypted or decoded."}}`, "error", false},
		{"text delta", history200FunctionFailure, "response.output_text.delta", false},
		{"duplicate key", `{"type":"error","code":"invalid_encrypted_content","code":"server_error"}`, "error", false},
		{"conflicting envelopes", `{"type":"response.failed","error":{"code":"invalid_encrypted_content"},"response":{"error":{"code":"server_error"}}}`, "response.failed", false},
		{"invalid JSON", `{"type":"error","code":"invalid_encrypted_content"`, "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.matched, isOpenAI200EncryptedHistoryRejection([]byte(tc.payload), tc.event))
		})
	}
}

func TestOpenAI200StreamGuards(t *testing.T) {
	for _, mode := range []string{"canceled", "committed", PlatformDeepseek, PlatformGrok, PlatformAnthropic, PlatformGemini, PlatformKimi, PlatformZhipu, "not_200"} {
		t.Run(mode, func(t *testing.T) {
			svc := newStreamLineageHTTPService(nil)
			enableHistory200(svc, true, false)
			c, rec := newStreamLineageTestContext("history200-guard")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = context.WithValue(ctx, openAIEncryptedContentRecoveryContextKey{}, openAIEncryptedContentRecoveryPolicy{body: history400Body(history400Input, true)})
			account := newStreamLineageHTTPAccount()
			response := newStreamLineageSSEResponse(encryptedSSERetryEvent(encryptedSSERetryFailure))
			switch mode {
			case "canceled":
				cancel()
			case "committed":
				_, _ = c.Writer.WriteString(": committed\n\n")
				c.Writer.Flush()
			case PlatformDeepseek, PlatformGrok, PlatformAnthropic, PlatformGemini, PlatformKimi, PlatformZhipu:
				account.Platform = mode
			case "not_200":
				response.StatusCode = http.StatusCreated
			}
			_, err := svc.handleStreamingResponseWithReasoning(ctx, response, c, account, time.Now(), "fixture", "fixture", "")
			require.Error(t, err)
			var signal *openAIEncryptedContentRecoverySignal
			require.NotErrorAs(t, err, &signal, "must not grant Beta recovery: %s", rec.Body.String())
		})
	}
}

type history200CountSettings struct {
	*history400SettingsRepo
	reads atomic.Int32
}

func (r *history200CountSettings) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyOpenAI200OmitEncryptedHistoryEnabled {
		r.reads.Add(1)
	}
	return r.history400SettingsRepo.GetValue(ctx, key)
}

func TestOpenAI200NormalStreamDoesNotReadSettingOrWaitForTerminal(t *testing.T) {
	svc := newStreamLineageHTTPService(nil)
	svc.cfg.Gateway.StreamDataIntervalTimeout = 30
	repo := &history200CountSettings{history400SettingsRepo: &history400SettingsRepo{values: map[string]string{SettingKeyOpenAI200OmitEncryptedHistoryEnabled: "true"}}}
	svc.settingService = NewSettingService(repo, svc.cfg)
	recorder := newOpenAIResponseFlushRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := context.WithValue(context.Background(), openAIEncryptedContentRecoveryContextKey{}, openAIEncryptedContentRecoveryPolicy{body: history400Body(history400Input, true)})
	allowTerminal := make(chan struct{})
	defer close(allowTerminal)
	reader := &stagedOpenAISSEReadCloser{
		segments: [][]byte{[]byte(streamLineageDeltaEvent), []byte(encryptedSSERetryEvent(history200Success))},
		gates:    []<-chan struct{}{nil, allowTerminal},
	}
	response := newStreamLineageSSEResponse("")
	response.Body = reader
	done := make(chan error, 1)
	go func() {
		_, err := svc.handleStreamingResponseWithReasoning(ctx, response, c, newStreamLineageHTTPAccount(), time.Now(), "fixture", "fixture", "")
		done <- err
	}()
	// If the handler waits for completion or a heartbeat, this cannot flush.
	waitOpenAIResponseFlushCount(t, recorder, 1)
	body, _ := recorder.snapshot()
	require.Contains(t, body, "response.output_text.delta")
	require.NotContains(t, body, "response.completed")
	require.Zero(t, repo.reads.Load(), "normal output must not add settings queries")
	// Release the reader before joining, including assertion-failure paths.
	t.Cleanup(func() { require.NoError(t, <-done) })
}

func TestOpenAI200RequiresNativeHTTPRecoveryPolicy(t *testing.T) {
	svc := newStreamLineageHTTPService(nil)
	enableHistory200(svc, true, false)
	c, _ := newStreamLineageTestContext("non-native")
	_, err := svc.handleStreamingResponseWithReasoning(context.Background(), newStreamLineageSSEResponse(encryptedSSERetryEvent(history200FunctionFailure)), c, newStreamLineageHTTPAccount(), time.Now(), "fixture", "fixture", "")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover, "the Beta must not change other adapters' existing error handling")
	require.Nil(t, openAI200HistoryDiagnostic(c))
}

func TestOpenAI200HistoryDiagnosticSnapshot(t *testing.T) {
	c, _ := newStreamLineageTestContext("history200-diagnostic")
	d := &OpenAI400HistoryDiagnostic{Applied: true, After409: true, Decision: "same_account_retry_scheduled"}
	c.Set(openAI200HistoryDiagnosticKey, d)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Kind: "retry", UpstreamStatusCode: 200, Message: "fixture"})
	d.Decision = "retry_exhausted"
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.Equal(t, "same_account_retry_scheduled", events[0].EncryptedHistory200.Decision)
	require.Nil(t, events[0].EncryptedHistory400)
	entry := &OpsInsertErrorLogInput{UpstreamErrors: events}
	require.NoError(t, sanitizeOpsUpstreamErrors(entry))
	require.Contains(t, *entry.UpstreamErrorsJSON, "encrypted_history_200")
	require.NotContains(t, *entry.UpstreamErrorsJSON, "encrypted_history_400")
	events[0].EncryptedHistory200 = &OpenAI400HistoryDiagnostic{Decision: "untrusted upstream text"}
	require.NoError(t, sanitizeOpsUpstreamErrors(entry))
	require.NotContains(t, *entry.UpstreamErrorsJSON, "untrusted")
}

func TestOpenAI200SettingDefaultsOff(t *testing.T) {
	var unavailable *SettingService
	require.False(t, unavailable.IsOpenAI200OmitEncryptedHistoryEnabled(context.Background()))
	for _, value := range []string{"", "false", "1", "TRUE", "true"} {
		settings := NewSettingService(&history400SettingsRepo{values: map[string]string{SettingKeyOpenAI200OmitEncryptedHistoryEnabled: value}}, nil)
		require.Equal(t, value == "true", settings.IsOpenAI200OmitEncryptedHistoryEnabled(context.Background()))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.False(t, settings.IsOpenAI200OmitEncryptedHistoryEnabled(ctx))
	}
}
