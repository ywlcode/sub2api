//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNativeMessagesSuccessfulTerminalPopulatesTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, miniAnthropicSSEStream())
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	collector := requesttiming.New(time.Now(), 0)
	ctx := requesttiming.With(context.Background(), collector)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, trace := requesttiming.StartAttempt(req, 165, 0)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	trace.Response(resp, nil)
	defer resp.Body.Close()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(req.Context())
	svc := newNativeAnthropicHangTestService(2)
	result, err := svc.handleNativeAnthropicStreamingResponse(ctx, resp, c, volcengineTestAccount(), "deepseek-flash-local", "deepseek-flash-local", "deepseek-flash", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !strings.Contains(rec.Body.String(), `"type":"message_stop"`) {
		t.Fatal("response failed before diagnostic assertion")
	}
	_ = resp.Body.Close()
	var snapshot requesttiming.Snapshot
	collector.WhenFinished(func(s requesttiming.Snapshot) { snapshot = s })
	collector.Finish(200, false)
	if len(snapshot.Attempts) != 1 {
		t.Fatalf("unexpected attempts: %d", len(snapshot.Attempts))
	}
	t.Logf("response is complete; trace terminal=%q attempt_terminal=%q", snapshot.Terminal, snapshot.Attempts[0].Terminal)
	if snapshot.Terminal != "completed" || snapshot.Attempts[0].Terminal != "completed" {
		t.Fatal("successful native Messages response does not mark trace terminal; cleanup cancellation can be misclassified as failure")
	}
}

func TestNativeMessagesTimingUsesValidatedCompleteEvents(t *testing.T) {
	textStream := miniAnthropicSSEStream()
	for _, tc := range []struct {
		name, stream, terminal string
		wantError, visible     bool
	}{
		{"complete", textStream, "completed", false, true},
		{"tools", toolAnthropicSSEStream() + "\n", "completed", false, true},
		{"error", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n", "failed", true, false},
		{"empty_thinking_eof", "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n", "", true, false},
		{"partial_terminal", strings.TrimRight(textStream, "\n"), "", true, true},
		{"terminal_without_stop_reason", "data: {\"type\":\"message_stop\"}\n\n", "", true, false},
		{"mismatched_terminal", strings.Replace(textStream, "event: message_stop", "event: ping", 1), "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := requesttiming.New(time.Now(), 0)
			ctx := requesttiming.With(context.Background(), collector)
			req := httptest.NewRequest(http.MethodPost, "http://fixture.test/messages", nil).WithContext(ctx)
			req, trace := requesttiming.StartAttempt(req, 165, 0)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.stream)), Request: req}
			trace.Response(resp, nil)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
			_, err := newNativeAnthropicHangTestService(0).handleNativeAnthropicStreamingResponse(ctx, resp, c, volcengineTestAccount(), "ds", "ds", "ds", nil, time.Now())
			require.Equal(t, tc.wantError, err != nil, "%v", err)
			require.NoError(t, resp.Body.Close())
			var snapshot requesttiming.Snapshot
			collector.WhenFinished(func(s requesttiming.Snapshot) { snapshot = s })
			collector.Finish(http.StatusOK, false)
			require.Equal(t, tc.terminal, snapshot.Terminal)
			require.Equal(t, tc.terminal, snapshot.Attempts[0].Terminal)
			_, visible := snapshot.Attempts[0].Events["first_visible"]
			require.Equal(t, tc.visible, visible)
			if tc.name == "empty_thinking_eof" {
				require.NotContains(t, snapshot.Attempts[0].Events, "first_reasoning")
			}
		})
	}
}

func TestNativeChatTimingRecordsContentAndCompletion(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete", false: "truncated"}[complete], func(t *testing.T) {
			payload := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"
			if complete {
				payload += "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
			}
			collector := requesttiming.New(time.Now(), 0)
			ctx := requesttiming.With(context.Background(), collector)
			req := httptest.NewRequest(http.MethodPost, "http://fixture.test/chat/completions", nil).WithContext(ctx)
			req, trace := requesttiming.StartAttempt(req, 165, 0)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload)), Request: req}
			trace.Response(resp, nil)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
			_, err := newNativeAnthropicHangTestService(0).streamRawChatCompletions(c, resp, volcengineTestAccount(), "ds", "ds", "ds", nil, nil, time.Now(), 100)
			require.Equal(t, !complete, err != nil, "%v", err)
			require.NoError(t, resp.Body.Close())
			var snapshot requesttiming.Snapshot
			collector.WhenFinished(func(s requesttiming.Snapshot) { snapshot = s })
			collector.Finish(200, false)
			require.Equal(t, complete, snapshot.Terminal == "completed")
			require.Equal(t, complete, snapshot.Attempts[0].Terminal == "completed")
			require.Contains(t, snapshot.Attempts[0].Events, "first_reasoning")
			require.Contains(t, snapshot.Attempts[0].Events, "first_visible_text")
		})
	}
}
