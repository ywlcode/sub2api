package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCompatibleUpstreamUploadHasDefaultDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream := apiKeyHealthTestHTTP{DoFunc: func(req *http.Request) (*http.Response, error) {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(121 * time.Second):
				return nil, errors.New("upload remained blocked past its deadline")
			}
		}}
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		ctx, release := detachUpstreamContext(context.Background())
		release()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/responses", strings.NewReader("large request"))
		require.NoError(t, err)
		_, err = svc.doOpenAIUpstream(req, "", &Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "upload")
	})
}

func TestUpstreamGuardPlatformsAndTLS(t *testing.T) {
	for _, platform := range []string{PlatformDeepseek, PlatformKimi, PlatformZhipu, PlatformMiniMax, PlatformGrok, PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformOpenCodeGo, PlatformMistral} {
		for _, tls := range []bool{false, true} {
			t.Run(platform+map[bool]string{false: "/http", true: "/tls"}[tls], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					upstream := apiKeyHealthTestHTTP{DoFunc: func(req *http.Request) (*http.Response, error) {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}}
					cfg := &config.Config{Gateway: config.GatewayConfig{UpstreamUploadTimeoutSeconds: 2}}
					guarded := gatewayHTTPUpstream(upstream, nil, &Account{Platform: platform}, cfg)
					req, err := http.NewRequest(http.MethodPost, "https://upstream.example", strings.NewReader("body"))
					require.NoError(t, err)
					start := time.Now()
					if tls {
						_, err = guarded.DoWithTLS(req, "", 1, 1, nil)
					} else {
						_, err = guarded.Do(req, "", 1, 1)
					}
					var timeout *upstreamRequestTimeout
					require.ErrorAs(t, err, &timeout)
					require.Equal(t, "upload", timeout.phase)
					require.Equal(t, 2*time.Second, time.Since(start))
				})
			})
		}
	}
}

func TestUpstreamGuardHeaderWaitAndLongResponse(t *testing.T) {
	for _, earlyResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload-complete", true: "early-response"}[earlyResponse], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{Gateway: config.GatewayConfig{UpstreamUploadTimeoutSeconds: 1, ResponseHeaderTimeout: 3}}
				req, _ := http.NewRequest(http.MethodPost, "https://upstream.example", strings.NewReader("body"))
				req, g := newUpstreamRequestGuard(req, cfg)
				defer g.close()
				if !earlyResponse {
					httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
				}
				synctest.Wait()
				if !earlyResponse {
					time.Sleep(2 * time.Second)
					require.NoError(t, req.Context().Err(), "upload timer must stop after WroteRequest")
				}
				resp, err := g.finish(req, &http.Response{Body: io.NopCloser(strings.NewReader("ok"))}, nil)
				require.NoError(t, err)
				defer resp.Body.Close()
				synctest.Wait()
				time.Sleep(time.Hour)
				require.NoError(t, req.Context().Err(), "connected response bodies have no total-duration limit")
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Gateway: config.GatewayConfig{UpstreamUploadTimeoutSeconds: 1, ResponseHeaderTimeout: 3}}
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example", strings.NewReader("body"))
		req, g := newUpstreamRequestGuard(req, cfg)
		defer g.close()
		httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		synctest.Wait()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		_, err := g.finish(req, nil, req.Context().Err())
		var timeout *upstreamRequestTimeout
		require.ErrorAs(t, err, &timeout)
		require.Equal(t, "response headers", timeout.phase)
	})
}

func TestUpstreamGuardDetachedDrainAndOwnerCancellation(t *testing.T) {
	for _, mode := range []string{"before-headers", "after-headers", "owner-revoked", "caller-deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				caller, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "caller-deadline" {
					caller, cancel = context.WithTimeout(context.Background(), time.Second)
					defer cancel()
				}
				owned, releaseOwner := WithAPIKeyAdmissionOwner(caller)
				defer releaseOwner()
				ctx, release := detachUpstreamContext(owned)
				release()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", strings.NewReader("body"))
				req, g := newUpstreamRequestGuard(req, nil)
				defer g.close()
				if mode == "after-headers" || mode == "owner-revoked" {
					resp, err := g.finish(req, &http.Response{Body: io.NopCloser(strings.NewReader("ok"))}, nil)
					require.NoError(t, err)
					defer resp.Body.Close()
				}
				synctest.Wait()
				if mode == "owner-revoked" {
					owner, _ := apiKeyAdmissionOwnerFromContext(owned)
					owner.cancel(ErrAPIKeySlotLeaseLost)
				} else if mode == "caller-deadline" {
					time.Sleep(time.Second)
				} else {
					cancel()
				}
				synctest.Wait()
				if mode == "after-headers" {
					time.Sleep(29 * time.Second)
					require.NoError(t, req.Context().Err(), "allow bounded billing drain")
					time.Sleep(2 * time.Second)
					synctest.Wait()
				}
				if mode == "owner-revoked" {
					require.ErrorIs(t, context.Cause(req.Context()), ErrAPIKeySlotLeaseLost)
				} else {
					require.ErrorIs(t, req.Context().Err(), context.Canceled)
				}
				g.close()
				select {
				case <-g.done:
				default:
					t.Fatal("guard watcher did not stop")
				}
			})
		})
	}
}

func TestUpstreamGuardHealthAttribution(t *testing.T) {
	for _, mode := range []string{"timeout", "client-cancel", "client-deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				limiter, account, cache, _ := newAPIKeyHealthFixture(t)
				account.Platform = PlatformDeepseek
				caller, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "client-deadline" {
					caller, cancel = context.WithTimeout(context.Background(), time.Second)
					defer cancel()
				}
				ctx, release := detachUpstreamContext(caller)
				release()
				upstream := apiKeyHealthTestHTTP{DoFunc: func(req *http.Request) (*http.Response, error) {
					if mode == "client-cancel" {
						cancel()
					}
					<-req.Context().Done()
					return nil, req.Context().Err()
				}}
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", strings.NewReader("body"))
				_, err := gatewayHTTPUpstream(upstream, limiter, account, nil).Do(req, "", account.ID, 1)
				require.Error(t, err)
				require.Equal(t, 1, cache.beginCalls)
				if mode == "timeout" {
					require.Equal(t, []apiKeyHealthSample{{false, 0}}, cache.samples)
				} else {
					require.Empty(t, cache.samples)
				}
			})
		})
	}
}

type guardTestBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *guardTestBody) Close() error { b.closed.Store(true); return nil }

func TestUpstreamGuardCleanupAndDisabledTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example", nil)
		req, g := newUpstreamRequestGuard(req, &config.Config{})
		time.Sleep(time.Hour)
		require.NoError(t, req.Context().Err(), "explicit zero disables timeout")
		body := &guardTestBody{Reader: strings.NewReader("ok")}
		resp, err := g.finish(req, &http.Response{Body: body}, nil)
		require.NoError(t, err)
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		select {
		case <-g.done:
		default:
			t.Fatal("EOF leaked watcher")
		}
		require.NoError(t, resp.Body.Close())
		require.NoError(t, resp.Body.Close())
		require.True(t, body.closed.Load())
	})
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ctx, release := detachUpstreamContext(ctx)
		release()
		body := &guardTestBody{Reader: strings.NewReader("body")}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", body)
		called := false
		upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) { called = true; return nil, nil }}
		_, err := gatewayHTTPUpstream(upstream, nil, &Account{Platform: PlatformDeepseek}, nil).Do(req, "", 1, 1)
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, called)
		require.True(t, body.closed.Load())
	})
}

// Exercise net/http's real blocked write, including request-body closure. No
// provider credentials or external network are used.
func TestUpstreamGuardStopsBlockedHTTPUpload(t *testing.T) {
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stop }))
	defer server.Close()
	defer close(stop)
	client := server.Client()
	client.Timeout = 5 * time.Second
	upstream := apiKeyHealthTestHTTP{DoFunc: client.Do}
	body := &guardTestBody{Reader: io.LimitReader(guardZeroReader{}, 64<<20)}
	req, err := http.NewRequest(http.MethodPost, server.URL, body)
	require.NoError(t, err)
	req.ContentLength = 64 << 20
	cfg := &config.Config{Gateway: config.GatewayConfig{UpstreamUploadTimeoutSeconds: 1}}
	start := time.Now()
	_, err = gatewayHTTPUpstream(upstream, nil, &Account{Platform: PlatformDeepseek}, cfg).Do(req, "", 1, 1)
	var timeout *upstreamRequestTimeout
	require.ErrorAs(t, err, &timeout)
	require.Equal(t, "upload", timeout.phase)
	require.Less(t, time.Since(start), 4*time.Second)
	require.Eventually(t, body.closed.Load, time.Second, 10*time.Millisecond)
}

type guardZeroReader struct{}

func (guardZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestUpstreamGuardClosesRealResponseAfterDisconnect(t *testing.T) {
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, release := detachUpstreamContext(caller)
	release()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("body"))
	cfg := &config.Config{Gateway: config.GatewayConfig{UpstreamUploadTimeoutSeconds: 2, UpstreamClientDisconnectGraceSeconds: 1}}
	upstream := apiKeyHealthTestHTTP{DoFunc: server.Client().Do}
	resp, err := gatewayHTTPUpstream(upstream, nil, &Account{Platform: PlatformDeepseek}, cfg).Do(req, "", 1, 1)
	require.NoError(t, err)
	defer resp.Body.Close()
	cancel()
	_, err = io.ReadAll(resp.Body)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream response connection leaked")
	}
}

func TestUpstreamGuardCompatibleProtocols(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions, APIProtocolAnthropic} {
		for _, mode := range []string{"upload-timeout", "client-cancel"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					caller, cancel := context.WithCancel(context.Background())
					defer cancel()
					var seenPath string
					upstream := apiKeyHealthTestHTTP{DoFunc: func(req *http.Request) (*http.Response, error) {
						seenPath = req.URL.Path
						if mode == "client-cancel" {
							cancel()
						}
						<-req.Context().Done()
						return nil, req.Context().Err()
					}}
					cfg := &config.Config{Gateway: config.GatewayConfig{UpstreamUploadTimeoutSeconds: 1}}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
					body := []byte(`{"model":"deepseek-flash","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))).WithContext(caller)
					baseURL := "https://upstream.example"
					if protocol == APIProtocolResponses {
						baseURL += "/v1"
					}
					account := &Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": baseURL, "api_key": "test", "api_protocol": protocol}}
					_, err := svc.ForwardAsAnthropic(caller, c, account, body, "", "")
					require.Equal(t, map[string]string{APIProtocolResponses: "/v1/responses", APIProtocolChatCompletions: "/v1/chat/completions", APIProtocolAnthropic: "/v1/messages"}[protocol], seenPath)
					if mode == "client-cancel" {
						require.ErrorIs(t, err, context.Canceled)
					} else {
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.Equal(t, http.StatusBadGateway, failover.StatusCode)
					}
				})
			})
		}
	}
}

func TestUpstreamGuardConcurrentCloseAndCancel(t *testing.T) {
	for i := 0; i < 40; i++ {
		caller, cancel := context.WithCancel(context.Background())
		ctx, release := detachUpstreamContext(caller)
		release()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example", nil)
		req, g := newUpstreamRequestGuard(req, nil)
		resp, err := g.finish(req, &http.Response{Body: io.NopCloser(strings.NewReader(""))}, nil)
		require.NoError(t, err)
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); cancel(); _ = resp.Body.Close() }()
		}
		wg.Wait()
		select {
		case <-g.done:
		default:
			t.Fatal("watcher survived concurrent close")
		}
	}
}

func TestCompatibleUpstreamClientCancellationBeforeHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, cancel := context.WithCancel(context.Background())
		defer cancel()
		upstream := apiKeyHealthTestHTTP{DoFunc: func(req *http.Request) (*http.Response, error) {
			cancel()
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(time.Second):
				return nil, errors.New("disconnected request still owns its upstream")
			}
		}}
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		ctx, release := detachUpstreamContext(client)
		release()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/responses", strings.NewReader("large request"))
		require.NoError(t, err)
		_, err = svc.doOpenAIUpstream(req, "", &Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey})
		require.ErrorIs(t, err, context.Canceled)
	})
}
