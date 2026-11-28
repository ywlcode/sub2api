//go:build unit

package service

import (
	"bytes"
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
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestPromptCacheShadowForwardHTTPAndSSE(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", endpoint, stream), func(t *testing.T) {
				var baselineBody, baselineWire []byte
				var baselineUsage OpenAIUsage
				for _, enabled := range []bool{false, true} {
					cfg := rawChatCompletionsTestConfig()
					cfg.Gateway.PromptCacheShadow = config.PromptCacheShadowConfig{Enabled: enabled, MinPrefixTokens: 32, PrefixTokensStep: 16}
					account := forceChatResponsesFallbackAccount()
					account.GroupIDs = []int64{2}
					upstream := &httpUpstreamRecorder{}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
					svc.channelService = NewChannelService(&shadowChannelPolicyTestRepository{accounts: []UpstreamChannelPromptCacheShadowAccount{{AccountID: account.ID, ChannelID: 11}}}, nil, nil, nil, nil)
					var events []map[string]any
					svc.promptCacheShadow.emit = func(fields []zap.Field) {
						enc := zapcore.NewMapObjectEncoder()
						for _, f := range fields {
							f.AddTo(enc)
						}
						events = append(events, enc.Fields)
					}
					turns := 1
					if enabled {
						turns = 2
					}
					for turn := 0; turn < turns; turn++ {
						text := strings.Repeat("Testing prompt cache prefix observation. ", 100)
						request := map[string]any{"model": "gpt-6.2-sol", "stream": stream}
						if endpoint == "responses" {
							request["input"] = text
						} else {
							request["messages"] = []any{map[string]any{"role": "user", "content": text}}
							if endpoint == "messages" {
								request["max_tokens"] = 64
							}
						}
						body, err := json.Marshal(request)
						require.NoError(t, err)
						upstreamBody := `{"id":"chatcmpl_shadow","object":"chat.completion","model":"gpt-6.2-sol","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2048,"completion_tokens":1,"total_tokens":2049,"prompt_tokens_details":{"cached_tokens":1920}}}`
						contentType := "application/json"
						if stream {
							contentType = "text/event-stream"
							upstreamBody = strings.Join([]string{
								`data: {"id":"chatcmpl_shadow","object":"chat.completion.chunk","model":"gpt-6.2-sol","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`, "",
								`data: {"id":"chatcmpl_shadow","object":"chat.completion.chunk","model":"gpt-6.2-sol","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2048,"completion_tokens":1,"total_tokens":2049,"prompt_tokens_details":{"cached_tokens":1920}}}`, "", "data: [DONE]", ""}, "\n")
						}
						upstream.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}, "x-request-id": []string{"rid_shadow_forward"}}, Body: io.NopCloser(strings.NewReader(upstreamBody))}
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest("POST", "/v1/"+endpoint, bytes.NewReader(body))
						c.Request.Header.Set("Content-Type", "application/json")
						groupID := int64(2)
						c.Set("api_key", &APIKey{ID: 7, GroupID: &groupID})
						var r *OpenAIForwardResult
						switch endpoint {
						case "responses":
							r, err = svc.Forward(context.Background(), c, account, body)
						case "chat":
							r, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
						case "messages":
							r, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
						}
						require.NoError(t, err)
						require.NotNil(t, r)
						require.Equal(t, 2048, r.Usage.InputTokens)
						require.Equal(t, 1920, r.Usage.CacheReadInputTokens)
						if !enabled {
							baselineBody = bytes.Clone(rec.Body.Bytes())
							baselineWire = bytes.Clone(upstream.lastBody)
							baselineUsage = r.Usage
						} else {
							if !stream && endpoint == "chat" {
								require.JSONEq(t, string(baselineBody), rec.Body.String())
							}
							require.JSONEq(t, string(baselineWire), string(upstream.lastBody))
							require.Equal(t, baselineUsage, r.Usage)
							require.Len(t, events, turn+1)
							row := events[turn]
							require.Equal(t, true, row["comparable"])
							require.Equal(t, "http", row["transport"])
							require.Equal(t, int64(1920), row["upstream_cache_read_tokens"])
							if turn == 0 {
								require.Equal(t, "cold", row["decision"])
							} else {
								require.Equal(t, "hit", row["decision"])
								require.Positive(t, row["simulated_cache_read_tokens"])
							}
						}
					}
				}
			})
		}
	}
}

func TestPromptCacheShadowNativeWebSocketWire(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.CloseNow() }()
				for turn := 0; turn < 2; turn++ {
					if _, _, err = ws.Read(ctx); err != nil {
						return
					}
					event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_shadow_%d","status":"completed","output":[],"usage":{"input_tokens":2048,"output_tokens":1,"input_tokens_details":{"cached_tokens":1920}}}}`, turn)
					if ws.Write(ctx, coderws.MessageText, []byte(event)) != nil {
						return
					}
				}
				<-ctx.Done()
			}))
			defer upstream.Close()
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.PromptCacheShadow = config.PromptCacheShadowConfig{Enabled: true, MinPrefixTokens: 32, PrefixTokensStep: 16}
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			account := passthroughLifecycleAccount()
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			account.Credentials["base_url"] = upstream.URL + "/v1"
			svc := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
			svc.channelService = NewChannelService(&shadowChannelPolicyTestRepository{accounts: []UpstreamChannelPromptCacheShadowAccount{{AccountID: account.ID, ChannelID: 11}}}, nil, nil, nil, nil)
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			svc.openaiWSPool = pool
			rows := make(chan map[string]any, 2)
			svc.promptCacheShadow.emit = func(fields []zap.Field) {
				enc := zapcore.NewMapObjectEncoder()
				for _, f := range fields {
					f.AddTo(enc)
				}
				rows <- enc.Fields
			}
			server, done := startPassthroughLifecycleServer(t, ctx, svc, account)
			defer server.Close()
			body, _ := json.Marshal(map[string]any{"type": "response.create", "model": "gpt-6.2-sol", "input": strings.Repeat("context ", 300)})
			client := dialPassthroughLifecycleClientWithPayload(t, server, string(body))
			defer func() { _ = client.CloseNow() }()
			for turn := 0; turn < 2; turn++ {
				if turn > 0 {
					require.NoError(t, client.Write(ctx, coderws.MessageText, body))
				}
				_, wire, err := client.Read(ctx)
				require.NoError(t, err)
				require.Contains(t, string(wire), "response.completed")
				select {
				case row := <-rows:
					require.Equal(t, true, row["comparable"])
					require.Equal(t, "ws", row["transport"])
					require.Equal(t, int64(1920), row["upstream_cache_read_tokens"])
					if turn == 0 {
						require.Equal(t, "cold", row["decision"])
					} else {
						require.Equal(t, "hit", row["decision"])
					}
				case <-ctx.Done():
					t.Fatal("missing shadow event")
				}
			}
			_ = client.CloseNow()
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("websocket did not stop")
			}
		})
	}
}

func BenchmarkPromptCacheShadowTokenizer(b *testing.B) {
	for _, size := range []int{4096, 100000, 1000000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			codec, err := openAIInputTokensCodecForModel("gpt-6.2-sol")
			if err != nil {
				b.Fatal(err)
			}
			text := strings.Repeat("a prompt cache observation test. ", size/32)
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := codec.Encode(text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
