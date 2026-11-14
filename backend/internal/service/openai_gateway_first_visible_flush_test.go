package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type firstVisibleFlushRecorder struct {
	*httptest.ResponseRecorder
	once       sync.Once
	firstFlush chan string
}

func (r *firstVisibleFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { r.firstFlush <- r.Body.String() })
}

func TestOpenAINativeFirstVisibleOutputFlushesAfterEmptyItem(t *testing.T) {
	// Queue a short upstream burst ahead of the consumer, including the blank
	// line after its first delta. The delta is processed with queueDrained=false.
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	gin.SetMode(gin.TestMode)
	previousSettings := gatewayForwardingCache.Load()
	if previousSettings == nil {
		previousSettings = (*cachedGatewayForwardingSettings)(nil)
	}
	defer gatewayForwardingCache.Store(previousSettings)
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: OpenAITTFTModeSemantic})

	for _, tc := range []struct {
		name, platform, item, delta string
	}{
		{
			name: "deepseek_reasoning", platform: "deepseek",
			item:  `{"type":"response.output_item.added","item":{"id":"rs_first","type":"reasoning","summary":[]}}`,
			delta: `{"type":"response.reasoning_summary_text.delta","item_id":"rs_first","delta":"thinking","summary_index":0,"output_index":0}`,
		},
		{
			name: "deepseek_text", platform: "deepseek",
			item:  `{"type":"response.output_item.added","item":{"id":"msg_first","type":"message","role":"assistant","content":[]}}`,
			delta: `{"type":"response.output_text.delta","item_id":"msg_first","delta":"hello","content_index":0,"output_index":0}`,
		},
		{
			name: "deepseek_tool_arguments", platform: "deepseek",
			item:  `{"type":"response.output_item.added","item":{"id":"fc_first","type":"function_call","call_id":"call_first","name":"lookup","arguments":""}}`,
			delta: `{"type":"response.function_call_arguments.delta","item_id":"fc_first","delta":"{}","output_index":0}`,
		},
		{
			name: "openai_reasoning", platform: PlatformOpenAI,
			item:  `{"type":"response.output_item.added","item":{"id":"rs_first","type":"reasoning","summary":[]}}`,
			delta: `{"type":"response.reasoning_summary_text.delta","item_id":"rs_first","delta":"thinking","summary_index":0,"output_index":0}`,
		},
		{
			name: "openai_text", platform: PlatformOpenAI,
			item:  `{"type":"response.output_item.added","item":{"id":"msg_first","type":"message","role":"assistant","content":[]}}`,
			delta: `{"type":"response.output_text.delta","item_id":"msg_first","delta":"hello","content_index":0,"output_index":0}`,
		},
	} {
		for _, async := range []bool{false, true} {
			mode := "sync"
			if async {
				mode = "async"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				releaseTerminal := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseTerminal) }) }
				defer release()

				firstEvent := "data: " + tc.delta + "\n\n"
				preamble := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_first\",\"created_at\":1}}\n\n" +
					"data: " + tc.item + "\n\n"
				writerDone := make(chan struct{})
				go func() {
					defer close(writerDone)
					defer writer.Close()
					if _, err := io.WriteString(writer, preamble+firstEvent); err != nil {
						return
					}
					// No further bytes, EOF or heartbeat may rescue a missing flush.
					<-releaseTerminal
					_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_first\",\"created_at\":1,\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
				}()

				recorder := &firstVisibleFlushRecorder{
					ResponseRecorder: httptest.NewRecorder(),
					firstFlush:       make(chan string, 1),
				}
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				cfg := &config.Config{}
				if async {
					cfg.Gateway.StreamDataIntervalTimeout = 30
				}
				svc := &OpenAIGatewayService{cfg: cfg}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
				type outcome struct {
					result *openaiStreamingResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c,
						&Account{ID: 1, Platform: tc.platform}, time.Now(), "test-model", "test-model")
					done <- outcome{result, err}
				}()

				var firstFlush string
				select {
				case firstFlush = <-recorder.firstFlush:
				case <-time.After(2 * time.Second):
				}
				release()
				select {
				case got := <-done:
					require.NoError(t, got.err)
					require.NotNil(t, got.result)
					require.NotNil(t, got.result.firstTokenMs)
					require.Equal(t, 2, got.result.usage.InputTokens)
					require.Equal(t, 1, got.result.usage.OutputTokens)
				case <-time.After(5 * time.Second):
					t.Fatal("stream handler did not exit")
				}
				select {
				case <-writerDone:
				case <-time.After(time.Second):
					t.Fatal("upstream writer did not exit")
				}
				require.NotEmpty(t, firstFlush, "first visible event must flush while upstream remains open and idle")
				require.Contains(t, firstFlush, firstEvent, "flush must include the complete first visible SSE event")
				require.True(t, strings.HasSuffix(firstFlush, "\n\n"), "flush must wait for the event boundary")
				require.NotContains(t, firstFlush, "response.completed")
			})
		}
	}
}
