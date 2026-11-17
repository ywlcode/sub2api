//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVolcengineContractResponsesNativeHistory(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","store":false,"input":[{"role":"user","content":"1+1"},{"type":"reasoning","id":"rs_native","summary":[{"type":"summary_text","text":"native thought"}],"encrypted_content":"native-state"},{"role":"assistant","content":"2"},{"role":"user","content":"plus one"}]}`)
	account := volcengineTestAccount()
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"3"}]}],"usage":{"input_tokens":10,"output_tokens":1}}`))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, w := volcengineUserContext(1, "/v1/responses", body)
	_, err := svc.Forward(context.Background(), c, account, body)
	t.Logf("status=%d upstream_calls=%d response=%s", w.Code, len(upstream.requests), w.Body.String())
	require.NoError(t, err, "native Ark history must reach its upstream")
	require.Equal(t, "native-state", gjson.GetBytes(upstream.lastBody, "input.1.encrypted_content").String())
}

func TestVolcengineContractNormalizerKeepsNativeCipher(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"native thought"}],"encrypted_content":"native-state"}]}`)
	got, err := normalizeVolcengineRequestBody(volcengineTestAccount(), body, APIProtocolResponses)
	require.NoError(t, err)
	t.Logf("normalized=%s", got)
	require.Equal(t, "native-state", gjson.GetBytes(got, "input.0.encrypted_content").String())
}

func TestVolcengineContractMessagesKeepsNativeSearchHistory(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","max_tokens":128,"messages":[{"role":"user","content":"search"},{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_native","name":"web_search","input":{"query":"test"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_native","content":[{"type":"web_search_result","url":"https://example.com","title":"test","encrypted_content":"native-result"}]},{"type":"text","text":"answer"}]},{"role":"user","content":"cite it again"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1}}`))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, w := volcengineUserContext(1, "/v1/messages", body)
	_, err := svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
	require.NoError(t, err, w.Body.String())
	t.Logf("forwarded_history=%s", gjson.GetBytes(upstream.lastBody, "messages.1.content").Raw)
	require.True(t, gjson.GetBytes(upstream.lastBody, `messages.1.content.#(type=="server_tool_use")`).Exists(), "native search call must survive")
	require.True(t, gjson.GetBytes(upstream.lastBody, `messages.1.content.#(type=="web_search_tool_result")`).Exists(), "native search result must survive")
}

func TestVolcengineContractResponsesKeepsToolOutputImage(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","store":false,"input":[{"type":"function_call","call_id":"call_img","name":"get_image","arguments":"{}"},{"type":"function_call_output","call_id":"call_img","output":[{"type":"input_text","text":"tool screenshot"},{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`)
	got := normalizeDeepSeekResponsesRequestBody(volcengineTestAccount(), body)
	t.Logf("normalized=%s", got)
	require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(got, "input").Raw, "native tool media should not be rewritten into a user turn")
}

func TestVolcengineContractMessagesFirstTokenMeasuresContent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		defer writer.Close()
		_, _ = io.WriteString(writer, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\n")
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(writer, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}()
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, w := volcengineUserContext(1, "/v1/messages", body)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
	require.NoError(t, err, w.Body.String())
	require.NotNil(t, result.FirstTokenMs)
	t.Logf("recorded_first_token_ms=%d actual_first_content_delay_ms=150", *result.FirstTokenMs)
	require.GreaterOrEqual(t, *result.FirstTokenMs, 100)
}

func TestVolcengineContractMessagesCapturesNativeSearchUsage(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","max_tokens":128,"messages":[{"role":"user","content":"search"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_search","type":"message","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1,"server_tool_use":{"web_search_requests":2}}}`))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, w := volcengineUserContext(1, "/v1/messages", body)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
	require.NoError(t, err, w.Body.String())
	t.Logf("upstream_search_requests=2 result_SearchCount=%d result_WebSearchCalls=%d", result.SearchCount, result.WebSearchCalls)
	require.NotNil(t, result.Usage.NativeToolUsage)
	require.NotNil(t, result.Usage.NativeToolUsage.WebSearchRequests)
	require.EqualValues(t, 2, *result.Usage.NativeToolUsage.WebSearchRequests)
	require.Zero(t, result.SearchCount)
	require.Zero(t, result.WebSearchCalls)
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 1, result.Usage.OutputTokens)
}

func TestVolcengineContractMessagesRequiresSemanticStopReason(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := []byte(`{"model":"deepseek-v4-1-flash-261017","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, w := volcengineUserContext(1, "/v1/messages", body)
	_, err := svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), body, "", "")
	t.Logf("http=%d missing_stop_reason_forward_error=%v", w.Code, err)
	require.Error(t, err, "message_stop alone is not a confirmed semantic completion")
}

func TestVolcengineContractMessagesStopReasons(t *testing.T) {
	for _, reason := range []string{"end_turn", "stop_sequence", "tool_use", "max_tokens", "", "unknown"} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%t", reason, enabled), func(t *testing.T) {
				account := volcengineTestAccount()
				account.Credentials[DeepSeekVolcengineCompatibilityCredential] = enabled
				payload := "event: message_delta\ndata: {\"type\":\"message_delta\",\ndata: \"delta\":{\"stop_reason\":\"" + reason + "\"},\"usage\":{\"output_tokens\":3}}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				body := []byte(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
				c, w := volcengineUserContext(1, "/v1/messages", body)
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				result, err := svc.handleNativeAnthropicStreamingResponse(context.Background(), resp, c, account, "deepseek-chat", "deepseek-chat", "deepseek-chat", nil, time.Now())
				if enabled && (reason == "" || reason == "unknown") {
					require.ErrorContains(t, err, "stop_reason")
					require.NotContains(t, w.Body.String(), "message_stop")
					require.Equal(t, 1, strings.Count(w.Body.String(), "event: error"))
				} else {
					require.NoError(t, err)
					require.Contains(t, w.Body.String(), "message_stop")
				}
				require.Equal(t, 4, result.Usage.OutputTokens)
				if enabled {
					require.Nil(t, result.FirstTokenMs, "metadata and terminals are not content")
				} else {
					require.NotNil(t, result.FirstTokenMs)
				}
			})
		}
	}
}

func TestVolcengineContractFirstContentKinds(t *testing.T) {
	for _, tc := range []struct {
		payload string
		content bool
	}{
		{`{"type":"message_start","message":{"content":[]}}`, false},
		{`{"type":"ping"}`, false},
		{`{"type":"content_block_start","content_block":{"type":"thinking","thinking":""}}`, false},
		{`{"type":"content_block_start","content_block":{"type":"text","text":""}}`, false},
		{`{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`, false},
		{`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"opaque"}}`, false},
		{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, true},
		{`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"plan"}}`, true},
		{`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}`, true},
		{`{"type":"content_block_start","content_block":{"type":"tool_use","name":"read"}}`, true},
		{`{"type":"content_block_start","content_block":{"type":"server_tool_use","name":"web_search"}}`, true},
		{`{"type":"content_block_start","content_block":{"type":"text","text":"hello"}}`, true},
		{`{"type":"content_block_start","content_block":{"type":"thinking","thinking":"plan"}}`, true},
	} {
		t.Run(tc.payload, func(t *testing.T) { require.Equal(t, tc.content, volcengineAnthropicHasContent(tc.payload)) })
	}
}

func TestVolcengineContractMessagesUsageSnapshots(t *testing.T) {
	usage := &ClaudeUsage{}
	parseVolcengineMessagesUsage(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3}}}`, usage)
	parseVolcengineMessagesUsage(`{"type":"message_delta","usage":{"output_tokens":4,"cache_read_input_tokens":0}}`, usage)
	parseVolcengineMessagesUsage(`{"type":"message_delta","usage":{"output_tokens":4,"input_tokens":null}}`, usage)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 4, usage.OutputTokens)
	require.Zero(t, usage.CacheReadInputTokens)
	for _, bad := range []string{`-1`, `1.5`, `"99"`, `9223372036854775808`, `null`} {
		parseVolcengineMessagesUsage(`{"type":"message_delta","usage":{"input_tokens":`+bad+`}}`, usage)
		require.Equal(t, 10, usage.InputTokens)
	}
}

func TestVolcengineContractSearchHistoryIsolation(t *testing.T) {
	body := []byte(strings.Replace(genuineWebSearchBody, `"model":"claude-sonnet-4-6"`, `"model":"deepseek-chat"`, 1))
	blocks := gjson.GetBytes([]byte(emulatedWebSearchBody), "messages.1.content").Array()
	var request map[string]any
	require.NoError(t, json.Unmarshal(body, &request))
	contents := request["messages"].([]any)[1].(map[string]any)["content"].([]any)
	for _, block := range blocks {
		contents = append(contents, json.RawMessage(block.Raw))
	}
	request["messages"].([]any)[1].(map[string]any)["content"] = contents
	body, err := json.Marshal(request)
	require.NoError(t, err)
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			account := volcengineTestAccount()
			account.Credentials[DeepSeekVolcengineCompatibilityCredential] = enabled
			account.Credentials["api_protocol"] = APIProtocolAnthropic
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"m","type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, w := volcengineUserContext(1, "/v1/messages", body)
			_, err := svc.forwardAnthropicViaNativeAnthropicEndpoint(context.Background(), c, account, body, "")
			require.NoError(t, err, w.Body.String())
			require.NotContains(t, string(upstream.lastBody), "srvtoolu_ws_")
			require.Equal(t, enabled, gjson.GetBytes(upstream.lastBody, `messages.1.content.#(type=="server_tool_use")`).Exists())
			require.Equal(t, enabled, gjson.GetBytes(upstream.lastBody, `messages.1.content.#(type=="web_search_tool_result")`).Exists())
			require.Contains(t, string(upstream.lastBody), "summary with citations")
		})
	}
	onlyEmulated := strings.ReplaceAll(emulatedWebSearchBody, `{"type":"text","text":"Here are the search results for \"weather\":"}`, `{"type":"server_tool_use","id":"srvtoolu_ws_other"}`)
	require.Contains(t, string(filterWebSearchHistoryBlocks([]byte(onlyEmulated), false)), "assistant content removed")
}

func TestVolcengineContractToolUsageSnapshots(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic} {
		observer := newVolcengineToolUsageObserver(volcengineTestAccount(), protocol)
		path := "tool_usage"
		field := "web_search"
		if protocol == APIProtocolAnthropic {
			path = "server_tool_use"
			field = "web_search_requests"
		}
		read := func(v string) { observer.observe([]byte(`{"usage":{"` + path + `":{"` + field + `":` + v + `}}}`)) }
		read("2")
		read("2")
		count := func() int64 {
			u := observer.snapshot()
			require.NotNil(t, u)
			if protocol == APIProtocolAnthropic {
				return *u.WebSearchRequests
			}
			return *u.WebSearch
		}
		require.EqualValues(t, 2, count(), "cumulative snapshots must not be added")
		for _, invalid := range []string{`-1`, `1.5`, `"4"`, `9223372036854775808`, `null`} {
			read(invalid)
			require.EqualValues(t, 2, count())
		}
		observer.observe([]byte(`{"usage":{"output_tokens":3}}`))
		require.EqualValues(t, 2, count())
		read("0")
		require.Zero(t, count())
	}
	for _, platform := range []string{PlatformDeepseek, PlatformOpenAI, PlatformKimi, PlatformAnthropic} {
		account := volcengineTestAccount()
		account.Platform = platform
		for _, enabled := range []bool{true, false} {
			account.Credentials[DeepSeekVolcengineCompatibilityCredential] = enabled
			observer := newVolcengineToolUsageObserver(account, APIProtocolResponses)
			observer.observe([]byte(`{"usage":{"tool_usage":{"web_search":2}}}`))
			require.Equal(t, platform == PlatformDeepseek && enabled, observer.snapshot() != nil)
		}
	}
}

func TestVolcengineContractNativeSearchDoesNotChangeBilling(t *testing.T) {
	var costs []float64
	for _, native := range []bool{false, true} {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		userRepo := &openAIRecordUsageUserRepoStub{}
		svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
		usage := OpenAIUsage{InputTokens: 1000, OutputTokens: 100}
		if native {
			n := int64(2)
			usage.NativeToolUsage = &VolcengineToolUsage{WebSearch: &n, WebSearchRequests: &n}
		}
		err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result: &OpenAIForwardResult{RequestID: "native-search-price", Model: openAICheapFixtureModel, Usage: usage, Duration: time.Second},
			APIKey: &APIKey{ID: 10}, User: &User{ID: 20}, Account: volcengineTestAccount(),
		})
		require.NoError(t, err)
		require.NotNil(t, usageRepo.lastLog)
		require.Positive(t, usageRepo.lastLog.ActualCost)
		costs = append(costs, usageRepo.lastLog.ActualCost)
	}
	require.Equal(t, costs[0], costs[1], "native diagnostic counters neither replace tokens nor charge Grok's default search tariff")
}

func TestVolcengineContractSSEHonorsConfiguredEventLimit(t *testing.T) {
	for _, probe := range []bool{false, true} {
		account := volcengineTestAccount()
		cfg := &config.Config{}
		cfg.Gateway.MaxLineSize = 256
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"x\":\"" + strings.Repeat("x", 512) + "\"}\n\n"))}}
		var transport HTTPUpstream = gatewayHTTPUpstream(upstream, nil, account, cfg)
		if probe {
			transport = (&AccountTestService{cfg: cfg, httpUpstream: upstream}).accountHTTPUpstream(account)
		}
		req, err := http.NewRequest(http.MethodPost, "https://ark.cn-beijing.volces.com/api/v3/responses", strings.NewReader(`{"input":"hi"}`))
		require.NoError(t, err)
		resp, err := transport.Do(req, "", account.ID, 1)
		require.NoError(t, err)
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		require.Error(t, err)
	}
	// Frame delimiters and many tiny lines also count toward the allocation cap.
	reader := newVolcengineSSEBodyWithLimit(io.NopCloser(strings.NewReader(strings.Repeat("x\n", 80)+"\n")), 128)
	defer reader.Close()
	_, err := io.ReadAll(reader)
	require.ErrorIs(t, err, errVolcengineSSEEventTooLarge)
}

func TestVolcengineContractToolUsageAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				payload := `{"id":"r","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":4,"output_tokens":1,"tool_usage":{"web_search":2,"knowledge_search":1}}}`
				path := "/v1/responses"
				body := fmt.Sprintf(`{"model":"ep-ark","input":"hello","store":false,"stream":%t}`, stream)
				if stream {
					payload = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
				}
				if protocol == APIProtocolChatCompletions {
					path = "/v1/chat/completions"
					body = fmt.Sprintf(`{"model":"ep-ark","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
					payload = `{"id":"c","object":"chat.completion","model":"ep-ark","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"tool_usage":{"web_search":2}}}`
					if stream {
						payload = "data: " + strings.ReplaceAll(payload, `"message":`, `"delta":`) + "\n\ndata: [DONE]\n\n"
					}
				} else if protocol == APIProtocolAnthropic {
					path = "/v1/messages"
					body = fmt.Sprintf(`{"model":"ep-ark","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
					payload = `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1,"server_tool_use":{"web_search_requests":2}}}`
					if stream {
						payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"server_tool_use\":{\"web_search_requests\":2}}}}\n\n" +
							"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1,\"server_tool_use\":{\"web_search_requests\":2}}}\n\n" +
							"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
				}
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				c, w := volcengineUserContext(1, path, []byte(body))
				var result *OpenAIForwardResult
				var err error
				switch protocol {
				case APIProtocolResponses:
					result, err = svc.Forward(context.Background(), c, volcengineTestAccount(), []byte(body))
				case APIProtocolChatCompletions:
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, volcengineTestAccount(), []byte(body), "", "")
				case APIProtocolAnthropic:
					result, err = svc.ForwardAsAnthropic(context.Background(), c, volcengineTestAccount(), []byte(body), "", "")
				}
				require.NoError(t, err, w.Body.String())
				require.NotNil(t, result)
				require.NotNil(t, result.Usage.NativeToolUsage)
				if protocol == APIProtocolAnthropic {
					require.EqualValues(t, 2, *result.Usage.NativeToolUsage.WebSearchRequests)
				} else {
					require.EqualValues(t, 2, *result.Usage.NativeToolUsage.WebSearch)
				}
				require.Zero(t, result.SearchCount)
				require.Zero(t, result.WebSearchCalls)
				require.Equal(t, 4, result.Usage.InputTokens)
				require.Equal(t, 1, result.Usage.OutputTokens)
				if protocol == APIProtocolResponses && stream {
					c, w = volcengineUserContext(1, path, []byte(strings.ReplaceAll(body, `"stream":true`, `"stream":false`)))
					resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
					buffered, err := svc.handleNonStreamingResponse(context.Background(), resp, c, volcengineTestAccount(), "ep-ark", "ep-ark")
					require.NoError(t, err, w.Body.String())
					require.EqualValues(t, 2, *buffered.usage.NativeToolUsage.WebSearch)
				}
			})
		}
	}
}

func TestVolcengineContractTerminalDoesNotWaitForEOF(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	sent := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		sent <- err
	}()
	c, w := volcengineUserContext(1, "/v1/messages", []byte(`{}`))
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	done := make(chan error, 1)
	go func() {
		_, err := svc.handleNativeAnthropicStreamingResponse(context.Background(), &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}, c, volcengineTestAccount(), "ep-ark", "ep-ark", "ep-ark", nil, time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		// Unblock both reader goroutines even when the regression reappears.
		require.NoError(t, writer.Close())
		<-done
		t.Fatal("a complete terminal must not wait for the upstream connection to close")
	}
	require.NoError(t, <-sent)
	require.Contains(t, w.Body.String(), "message_stop")
	require.NotContains(t, w.Body.String(), "event: error")
}
