package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func volcengineCompactionTestBody(t *testing.T, path string, stream bool) []byte {
	t.Helper()
	request := map[string]any{"model": "ep-ark", "stream": stream, "tools": []any{}}
	if path == "/v1/responses" {
		request["input"] = []any{
			map[string]any{"type": "message", "role": "user", "content": "Project Lantern. Keep the 24-hour window. Never deploy."},
			map[string]any{"type": "message", "role": "assistant", "content": "OLD_ASSISTANT_ANSWER"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": volcengineCompactionPrompt}}},
		}
		request["reasoning"] = map[string]any{"effort": "high", "summary": "auto"}
		request["max_output_tokens"] = 8192
	} else {
		request["messages"] = []any{
			map[string]any{"role": "user", "content": "Project Lantern. Keep the 24-hour window. Never deploy."},
			map[string]any{"role": "assistant", "content": "OLD_ASSISTANT_ANSWER"},
			map[string]any{"role": "user", "content": volcengineCompactionPrompt},
		}
		request["output_config"] = map[string]any{"effort": "high"}
		request["max_tokens"] = 8192
	}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	return body
}

func volcengineCompactionTestResponse(t *testing.T, text, status string, stream bool) *http.Response {
	t.Helper()
	reasoning := apicompat.ResponsesOutput{Type: "reasoning", ID: "rs_test", Status: "completed", Summary: []apicompat.ResponsesSummary{{Type: "summary_text", Text: "THOUGHT_MUST_NOT_BECOME_SUMMARY"}}}
	final := &apicompat.ResponsesResponse{ID: "resp_test", Object: "response", CreatedAt: 1, Model: "ep-ark", Status: "completed", Output: []apicompat.ResponsesOutput{reasoning}, Usage: &apicompat.ResponsesUsage{InputTokens: 10, OutputTokens: 7, TotalTokens: 17, InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: 3}, OutputTokensDetails: &apicompat.ResponsesOutputTokensDetails{ReasoningTokens: 4}}}
	if text != "" {
		final.Output = append(final.Output, apicompat.ResponsesOutput{Type: "message", ID: "msg_test", Role: "assistant", Status: "completed", Content: []apicompat.ResponsesContentPart{{Type: "output_text", Text: text}}})
	}
	terminal := "response.completed"
	switch status {
	case "max_output_tokens", "content_filter":
		final.Status, terminal = "incomplete", "response.incomplete"
		final.IncompleteDetails = &apicompat.ResponsesIncompleteDetails{Reason: status}
	case "failed":
		final.Status, terminal = "failed", "response.failed"
		final.Error = &apicompat.ResponsesError{Code: "invalid_request_error", Message: "Invalid input history"}
	case "cancelled":
		final.Status, terminal = "cancelled", "response.cancelled"
	}
	payload, err := json.Marshal(final)
	require.NoError(t, err)
	contentType := "application/json"
	if stream {
		events := []apicompat.ResponsesStreamEvent{
			{Type: "response.created", Response: &apicompat.ResponsesResponse{ID: final.ID, Status: "in_progress"}},
			{Type: "response.reasoning_summary_text.delta", Delta: reasoning.Summary[0].Text},
			{Type: "response.output_item.done", Item: &reasoning},
		}
		if text != "" {
			item := final.Output[1]
			events = append(events, apicompat.ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 1, Delta: text}, apicompat.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 1, Item: &item})
		}
		if status != "eof" {
			events = append(events, apicompat.ResponsesStreamEvent{Type: terminal, Response: final})
		}
		var wire bytes.Buffer
		for i := range events {
			events[i].SequenceNumber = i
			data, marshalErr := json.Marshal(events[i])
			require.NoError(t, marshalErr)
			fmt.Fprintf(&wire, "event: %s\ndata: %s\n\n", events[i].Type, data)
		}
		payload, contentType = wire.Bytes(), "text/event-stream"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(payload))}
}

func TestVolcengineCompactionDetection(t *testing.T) {
	body := volcengineCompactionTestBody(t, "/v1/responses", true)
	require.True(t, isVolcengineTextCompaction(body))
	for _, tc := range []struct {
		path  string
		value any
	}{
		{"tools", []any{map[string]any{"type": "function", "name": "lookup"}}},
		{"tools", "invalid"},
		{"input.2.role", "assistant"},
		{"input.2.content.0.text", "Explain this quoted prompt:\n" + volcengineCompactionPrompt},
		{"input.2.content.0.text", "Summarize our work so far."},
		{"input.2.content.0.type", "input_image"},
	} {
		changed, err := sjson.SetBytes(body, tc.path, tc.value)
		require.NoError(t, err)
		require.False(t, isVolcengineTextCompaction(changed), tc.path)
	}
	changed, err := sjson.SetBytes(body, "input.2.content", volcengineCompactionPrompt)
	require.NoError(t, err)
	require.True(t, isVolcengineTextCompaction(changed))
}

func TestVolcengineCompactionRecoveryAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"normal", "thinking_only", "empty", "truncated", "eof", "both_fail", "policy", "failed", "cancelled"} {
				name := fmt.Sprintf("%s/stream=%t/%s", path, stream, scenario)
				t.Run(name, func(t *testing.T) {
					upstreamStream := stream || path == "/v1/messages"
					firstText, firstStatus := "VALID_SUMMARY", "completed"
					wantCalls, wantError := 1, false
					switch scenario {
					case "thinking_only", "empty", "both_fail":
						firstText, wantCalls = "", 2
						wantError = scenario == "both_fail"
					case "truncated":
						firstText, firstStatus, wantCalls = "PARTIAL_DRAFT", "max_output_tokens", 2
					case "eof":
						if !upstreamStream {
							t.Skip("EOF applies to streaming upstream")
						}
						firstText, firstStatus, wantCalls = "PARTIAL_DRAFT", "eof", 2
					case "policy":
						firstText, firstStatus, wantError = "PARTIAL_DRAFT", "content_filter", true
					case "failed", "cancelled":
						firstText, firstStatus, wantError = "", scenario, true
					}
					first := volcengineCompactionTestResponse(t, firstText, firstStatus, upstreamStream)
					if scenario == "empty" {
						first.Body = io.NopCloser(strings.NewReader(`{"id":"r","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}`))
						first.Header.Set("Content-Type", "application/json")
					}
					secondText := "VALID_SUMMARY"
					if scenario == "both_fail" {
						secondText = ""
					}
					upstream := &httpUpstreamRecorder{responses: []*http.Response{first, volcengineCompactionTestResponse(t, secondText, "completed", upstreamStream)}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
					body := volcengineCompactionTestBody(t, path, stream)
					c, recorder := volcengineUserContext(1, path, body)
					var result *OpenAIForwardResult
					var err error
					if path == "/v1/responses" {
						result, err = svc.Forward(context.Background(), c, volcengineTestAccount(), body)
					} else {
						result, err = svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
					}
					require.Len(t, upstream.requests, wantCalls)
					require.Equal(t, "high", gjson.GetBytes(upstream.bodies[0], "reasoning.effort").String())
					if wantCalls == 2 {
						require.Equal(t, "none", gjson.GetBytes(upstream.bodies[1], "reasoning.effort").String())
						require.Equal(t, gjson.GetBytes(upstream.bodies[0], "input").Raw, gjson.GetBytes(upstream.bodies[1], "input").Raw)
						require.Equal(t, gjson.GetBytes(upstream.bodies[0], "max_output_tokens").Int(), gjson.GetBytes(upstream.bodies[1], "max_output_tokens").Int())
					}
					wire := recorder.Body.String()
					require.NotContains(t, wire, "THOUGHT_MUST_NOT_BECOME_SUMMARY")
					require.NotContains(t, wire, "PARTIAL_DRAFT")
					if wantError {
						require.Error(t, err)
						var failover *UpstreamFailoverError
						require.False(t, errors.As(err, &failover), "recovery must not enter another retry/scheduling loop")
						require.Contains(t, wire, "did not produce a complete text summary")
						require.NotContains(t, wire, "response.completed")
						require.NotContains(t, wire, "message_stop")
						if !stream {
							require.GreaterOrEqual(t, recorder.Code, 400)
						}
					} else {
						require.NoError(t, err, wire)
						require.NotNil(t, result)
						require.Contains(t, wire, "VALID_SUMMARY")
						expectedTokens := 10 * wantCalls
						if scenario == "eof" {
							expectedTokens = 10
						}
						require.Equal(t, expectedTokens, result.Usage.InputTokens)
						if stream && path == "/v1/responses" {
							require.Equal(t, 1, strings.Count(wire, "event: response.completed"))
						}
					}
					if capture := os.Getenv("SUB2API_COMPACTION_CAPTURE_DIR"); capture != "" && stream && path == "/v1/responses" {
						require.NoError(t, os.MkdirAll(capture, 0700))
						require.NoError(t, os.WriteFile(filepath.Join(capture, scenario+".sse"), recorder.Body.Bytes(), 0600))
					}
				})
			}
		}
	}
}

func TestVolcengineCompactionStrictScope(t *testing.T) {
	for _, scope := range []string{"flag_off", "other_platform", "ordinary_request"} {
		t.Run(scope, func(t *testing.T) {
			account := volcengineTestAccount()
			body := volcengineCompactionTestBody(t, "/v1/responses", false)
			switch scope {
			case "flag_off":
				account.Credentials[DeepSeekVolcengineCompatibilityCredential] = false
			case "other_platform":
				account.Platform = PlatformOpenAI
			case "ordinary_request":
				body, _ = sjson.SetBytes(body, "input.2.content", "Ordinary question about summary fields")
			}
			upstream := &httpUpstreamRecorder{resp: volcengineCompactionTestResponse(t, "", "completed", false)}
			req, err := http.NewRequest(http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
			require.NoError(t, err)
			resp, err := accountVolcengineHTTP(upstream, account).Do(req, "", account.ID, 1)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			wire, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(wire), "THOUGHT_MUST_NOT_BECOME_SUMMARY")
			require.Len(t, upstream.requests, 1)
		})
	}
}

type volcengineCompactionBlockingBody struct {
	done chan struct{}
	once sync.Once
}

func (b *volcengineCompactionBlockingBody) Read([]byte) (int, error) { <-b.done; return 0, io.EOF }
func (b *volcengineCompactionBlockingBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return nil
}

func TestVolcengineCompactionCancellationClosesBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	body := volcengineCompactionTestBody(t, "/v1/responses", true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
	require.NoError(t, err)
	blocked := &volcengineCompactionBlockingBody{done: make(chan struct{})}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: blocked}}
	_, err = accountVolcengineHTTP(upstream, volcengineTestAccount()).Do(req, "", 144, 1)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "cancellation must not submit a second request")
	select {
	case <-blocked.done:
	default:
		t.Fatal("canceled compaction did not close upstream body")
	}
}

func TestVolcengineCompactionSnapshotAndDeltaValidation(t *testing.T) {
	for _, scenario := range []string{"snapshot_only", "delta_only", "oversized", "conflicting_snapshot", "refusal", "tool_call"} {
		t.Run(scenario, func(t *testing.T) {
			resp := volcengineCompactionTestResponse(t, "summary", "completed", false)
			switch scenario {
			case "snapshot_only":
				resp = volcengineCompactionTestResponse(t, "summary", "completed", true)
				wire, _ := io.ReadAll(resp.Body)
				lines := strings.Split(string(wire), "\n")
				var terminal []string
				for _, line := range lines {
					if strings.Contains(line, `"type":"response.completed"`) {
						terminal = append(terminal, "event: response.completed", line, "", "")
					}
				}
				resp.Body = io.NopCloser(strings.NewReader(strings.Join(terminal, "\n")))
			case "delta_only", "conflicting_snapshot":
				resp = volcengineCompactionTestResponse(t, "summary", "completed", true)
				wire, _ := io.ReadAll(resp.Body)
				if scenario == "delta_only" {
					var rebuilt strings.Builder
					for _, line := range strings.Split(string(wire), "\n") {
						if strings.HasPrefix(line, "data:") && strings.Contains(line, `"type":"response.completed"`) {
							data, _ := sjson.SetRaw(line[5:], "response.output", `[]`)
							line = "data: " + data
						}
						_, _ = rebuilt.WriteString(line + "\n")
					}
					resp.Body = io.NopCloser(strings.NewReader(rebuilt.String()))
				} else {
					resp.Body = io.NopCloser(strings.NewReader(strings.Replace(string(wire), `"delta":"summary"`, `"delta":"different"`, 1)))
				}
			default:
				wire, _ := io.ReadAll(resp.Body)
				switch scenario {
				case "oversized":
					wire, _ = sjson.SetBytes(wire, "output.1.content.0.text", strings.Repeat("x", volcengineCompactionMaxTextBytes+1))
				case "refusal":
					wire, _ = sjson.SetBytes(wire, "output.1.content.0.type", "refusal")
				case "tool_call":
					wire, _ = sjson.SetBytes(wire, "output.1.type", "function_call")
				}
				resp.Body = io.NopCloser(bytes.NewReader(wire))
			}
			defer func() { _ = resp.Body.Close() }()
			final, _, reason := readVolcengineCompaction(resp)
			if scenario == "snapshot_only" || scenario == "delta_only" {
				require.Empty(t, reason)
				require.Equal(t, "summary", final.Output[0].Content[0].Text)
			} else {
				require.NotEmpty(t, reason)
			}
		})
	}
}

func TestVolcengineCompactionOutputCap(t *testing.T) {
	for _, cap := range []int{0, 17} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			body := volcengineCompactionTestBody(t, "/v1/responses", false)
			if cap == 0 {
				body, _ = sjson.DeleteBytes(body, "max_output_tokens")
			} else {
				body, _ = sjson.SetBytes(body, "max_output_tokens", cap)
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				volcengineCompactionTestResponse(t, "", "completed", false),
				volcengineCompactionTestResponse(t, "summary", "completed", false),
			}}
			req, err := http.NewRequest(http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
			require.NoError(t, err)
			resp, err := accountVolcengineHTTP(upstream, volcengineTestAccount()).Do(req, "", 144, 1)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Len(t, upstream.requests, 2)
			want := int64(cap)
			if cap == 0 {
				want = volcengineCompactionDefaultTokens
			}
			for _, sent := range upstream.bodies {
				require.Equal(t, want, gjson.GetBytes(sent, "max_output_tokens").Int())
			}
			require.NoError(t, resp.Request.Context().Err(), "helper timeout must not cancel the returned response context")
		})
	}
}

func TestVolcengineCompactionRecoveryBeforeGzip(t *testing.T) {
	account := volcengineTestAccount()
	account.Extra = map[string]any{AccountExtraRequestGzipEnabled: true}
	body := volcengineCompactionTestBody(t, "/v1/responses", true)
	body, _ = sjson.SetBytes(body, "input.0.content", strings.Repeat("synthetic context ", 1024))
	var sent [][]byte
	upstream := gzipTestHTTP{send: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
		reader, err := gzip.NewReader(req.Body)
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		sent = append(sent, data)
		text := ""
		if len(sent) == 2 {
			text = "summary"
		}
		return volcengineCompactionTestResponse(t, text, "completed", true), nil
	}}
	req, err := http.NewRequest(http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := accountVolcengineHTTP(accountRequestGzipHTTP(upstream, account), account).Do(req, "", account.ID, 1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Len(t, sent, 2)
	require.Equal(t, "high", gjson.GetBytes(sent[0], "reasoning.effort").String())
	require.Equal(t, "none", gjson.GetBytes(sent[1], "reasoning.effort").String())
	require.Equal(t, gjson.GetBytes(sent[0], "input").Raw, gjson.GetBytes(sent[1], "input").Raw)
}

func TestVolcengineCompactionHeartbeatAndFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			calls := 0
			upstream := gzipTestHTTP{send: func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					time.Sleep(1100 * time.Millisecond)
				}
				return volcengineCompactionTestResponse(t, "", "completed", true), nil
			}}
			cfg := &config.Config{}
			cfg.Gateway.StreamKeepaliveInterval = 1
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &volcengineNoReasoningCache{}}
			body := volcengineCompactionTestBody(t, path, true)
			c, recorder := volcengineUserContext(1, path, body)
			var err error
			if path == "/v1/responses" {
				stop := WrapOpenAIResponsesHeaderWait(c, true)
				defer stop()
				_, err = svc.Forward(context.Background(), c, volcengineTestAccount(), body)
			} else {
				_, err = svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
			}
			require.Error(t, err)
			require.Equal(t, 2, calls)
			wire := recorder.Body.String()
			require.Contains(t, wire, ": keepalive\n\n")
			require.Contains(t, wire, "did not produce a complete text summary")
			require.NotContains(t, wire, "response.completed")
			require.NotContains(t, wire, "message_stop")
			require.NotContains(t, wire, "THOUGHT_MUST_NOT_BECOME_SUMMARY")
			if path == "/v1/messages" {
				require.Contains(t, wire, "event: error")
			} else {
				require.Contains(t, wire, "event: response.failed")
			}
		})
	}
}

func TestVolcengineCompactionDoesNotRetryBarePolicyError(t *testing.T) {
	body := volcengineCompactionTestBody(t, "/v1/responses", true)
	req, err := http.NewRequest(http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
	require.NoError(t, err)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"invalid_request_error\",\"message\":\"Invalid request history\"}}\n\n"))}}
	_, err = accountVolcengineHTTP(upstream, volcengineTestAccount()).Do(req, "", 144, 1)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
}

type volcengineCompactionDelayedReader struct {
	io.Reader
	once sync.Once
}

func (r *volcengineCompactionDelayedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { time.Sleep(1100 * time.Millisecond) })
	return r.Reader.Read(p)
}

func TestVolcengineCompactionHeaderTimeoutStopsBeforeSummaryBody(t *testing.T) {
	body := volcengineCompactionTestBody(t, "/v1/responses", true)
	upstream := gzipTestHTTP{send: func(req *http.Request) (*http.Response, error) {
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		resp := volcengineCompactionTestResponse(t, "summary", "completed", true)
		resp.Body = io.NopCloser(&volcengineCompactionDelayedReader{Reader: resp.Body})
		return resp, nil
	}}
	cfg := &config.Config{}
	cfg.Gateway.ResponseHeaderTimeout = 1
	req, err := http.NewRequest(http.MethodPost, "https://ark.example/api/v3/responses", bytes.NewReader(body))
	require.NoError(t, err)
	resp, err := gatewayHTTPUpstream(upstream, nil, volcengineTestAccount(), cfg).Do(req, "", 144, 1)
	require.NoError(t, err, "summary-body wait must not count as waiting for response headers")
	defer func() { _ = resp.Body.Close() }()
	wire, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(wire), "summary")
}
