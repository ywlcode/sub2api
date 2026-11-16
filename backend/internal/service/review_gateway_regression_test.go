//go:build unit

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestReviewHealthCloseWithoutReadingIsBounded(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprint(slow), func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"error":{"code":"context_length_exceeded"}}`))
			if slow {
				reader, writer := io.Pipe()
				defer func() { _ = writer.Close() }()
				body = reader
			}
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
			}}
			resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(httptest.NewRequest(http.MethodPost, "https://upstream.example", nil), "", account.ID, 1)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { done <- resp.Body.Close() }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("Close did not interrupt pending health inspection")
			}
			if !slow {
				require.Empty(t, cache.samples, "closing a known client rejection must not trip account health")
			}
		})
	}
}

func TestReviewNativeSSEMultilineErrorStaysWhole(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("event: error\ndata: {\ndata: \"type\":\"error\",\ndata: \"error\":{\"code\":\"cyber_policy\"}}\n\n"))
	pump := newAnthropicNativeLinePump(scanner, time.Second)
	defer pump.stop()
	data, err := pump.nextData(4096)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"error","error":{"code":"cyber_policy"}}`, data)
	require.True(t, isCyberPolicyRejection([]byte(data)), "multiline framing must not bypass policy detection")
}

func TestReviewRawChatForbiddenAfterOutputRetainsFreeFailurePolicy(t *testing.T) {
	for _, code := range []string{"cyber_policy", "content_blocked"} {
		t.Run(code, func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			bindForbiddenTestRule(c, "permission")
			body := `data: {"id":"chat-review","choices":[{"index":0,"delta":{"content":"partial answer"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n\n" +
				fmt.Sprintf(`data: {"error":{"type":"permission_error","code":%q,"message":"private upstream detail"}}`, code) + "\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
			svc := &OpenAIGatewayService{}
			result, err := svc.streamRawChatCompletions(c, resp, &Account{ID: 144, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "gpt-test", "gpt-test", "gpt-test", nil, nil, time.Now(), 10)
			require.ErrorIs(t, err, errOpenAIErrorPassthroughForwarded)
			require.Nil(t, result, "do not change the existing free rejected-request billing policy")
			require.Contains(t, rec.Body.String(), "partial answer")
			require.NotContains(t, rec.Body.String(), "private upstream detail")
			if code == "cyber_policy" {
				require.NotNil(t, GetOpsCyberPolicy(c), "free failure must retain internal audit evidence")
			}
		})
	}
}

func TestReviewSlowHealthErrorBodyRemainsReadable(t *testing.T) {
	for _, code := range []string{"context_length_exceeded", "server_error"} {
		t.Run(code, func(t *testing.T) {
			limiter, account, cache, _ := newAPIKeyHealthFixture(t)
			reader, writer := io.Pipe()
			release := make(chan struct{})
			done := make(chan struct{})
			defer func() { _ = reader.Close() }()
			go func() {
				defer close(done)
				defer func() { _ = writer.Close() }()
				_, _ = io.WriteString(writer, `{"error":{"message":"slow error",`)
				<-release
				_, _ = fmt.Fprintf(writer, `"code":%q}}`, code)
			}()
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}}, Body: reader}, nil
			}}
			req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
			start := time.Now()
			resp, err := apiKeyHealthHTTP(upstream, limiter, account).Do(req, "", account.ID, 1)
			close(release)
			require.NoError(t, err)
			require.Less(t, time.Since(start), 3*time.Second, "health inspection must yield to the real consumer")
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			<-done
			require.NoError(t, readErr, "health observation must not close the upstream body")
			require.JSONEq(t, fmt.Sprintf(`{"error":{"message":"slow error","code":%q}}`, code), string(body))
			if code == "context_length_exceeded" {
				require.Empty(t, cache.samples, "a late client error must stay neutral")
			} else {
				require.Equal(t, []apiKeyHealthSample{{false, 500}}, cache.samples)
			}
		})
	}
}

func TestReviewRemoteBillingSupportsTwentyGroups(t *testing.T) {
	for _, size := range []int{5, 6, 20, 21} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var doc map[string]any
			require.NoError(t, json.Unmarshal(multiGroupBillingBody(t), &doc))
			existing, ok := doc["groups"].([]any)
			require.True(t, ok)
			prototype, ok := existing[0].(map[string]any)
			require.True(t, ok)
			groups := make([]map[string]any, size)
			for i := range groups {
				group := make(map[string]any)
				for key, value := range prototype {
					group[key] = value
				}
				group["group_id"] = i + 1
				groups[i] = group
			}
			doc["groups"] = groups
			body, err := json.Marshal(doc)
			require.NoError(t, err)
			parsed, err := parseUpstreamBillingProbeResponse(body)
			if size > 20 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, parsed["groups"], size)
			require.Equal(t, 5, maxAPIKeyGroups, "remote capability must not expand local Key grants")
		})
	}
}

func TestReviewDeepSeekHostPreservesRawReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const terminal = `data: {"type":"response.completed","response":{"id":"resp-review","status":"completed","model":"deepseek-chat","output":[{"id":"rs1","type":"reasoning","content":[{"type":"reasoning_text","text":"complete reasoning"}],"summary":[{"type":"summary_text","text":"short summary"}]},{"id":"msg1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":8,"output_tokens":12}}}` + "\n\n"
	for _, host := range []string{"https://api.deepseek.com", "https://api.openai.com"} {
		for _, protocol := range []string{"chat", "messages"} {
			t.Run(host+"/"+protocol, func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(terminal))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": host, "api_key": "test"}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				body := []byte(`{"model":"deepseek-chat","stream":false,"max_tokens":100,"messages":[{"role":"user","content":"test"}]}`)
				var result *OpenAIForwardResult
				var err error
				if protocol == "chat" {
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				} else {
					result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				if host == "https://api.deepseek.com" {
					require.Contains(t, rec.Body.String(), "complete reasoning")
					require.NotContains(t, rec.Body.String(), "short summary")
				} else {
					require.Contains(t, rec.Body.String(), "short summary")
				}
			})
		}
	}
}
