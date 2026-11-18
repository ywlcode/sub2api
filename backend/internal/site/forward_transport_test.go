package site

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Exercise the actual forwarding transport, including the case where HTTP 200
// has arrived but the connection silently stops answering. No production socket
// or firewall is touched; synctest advances the real configured timeout values.
func TestSiteForwardDetectsSilentHTTP2Connection(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_headers", true: "after_output"}[headers], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: "https://primary.test", Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
				require.NoError(t, err)
				defer m.Stop()
				clientConn, serverConn := net.Pipe()
				defer func() { _ = clientConn.Close() }()
				defer func() { _ = serverConn.Close() }()
				go func() { _ = serveForwardPingPeer(serverConn, -1, time.Hour, headers) }()
				tr := m.forwardTransport
				tr.Protocols = new(http.Protocols)
				tr.Protocols.SetUnencryptedHTTP2(true)
				tr.DialContext = func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }
				client := &http.Client{Transport: m.proxy.Transport, Timeout: time.Minute}
				start := time.Now()
				resp, err := client.Post("http://primary.test/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-test","stream":true}`))
				if headers {
					require.NoError(t, err)
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					require.Equal(t, "data: first\n\n", string(body))
					err = readErr
				}
				require.ErrorContains(t, err, "http2: client connection lost")
				require.Equal(t, 30*time.Second, time.Since(start))
			})
		})
	}
}

type forwardWireFixture struct {
	m           *Manager
	client      *http.Client
	dials       atomic.Int32
	mu          sync.Mutex
	connections []net.Conn
}

func newForwardWireFixture(t *testing.T, peers ...*forwardPeer) *forwardWireFixture {
	t.Helper()
	m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: "https://primary.test", Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
	require.NoError(t, err)
	f := &forwardWireFixture{m: m}
	tr := m.forwardTransport
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetUnencryptedHTTP2(true)
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		n := int(f.dials.Add(1)) - 1
		peer := peers[min(n, len(peers)-1)]
		client, server := net.Pipe()
		f.mu.Lock()
		f.connections = append(f.connections, client, server)
		f.mu.Unlock()
		go func() { _ = peer.serve(server) }()
		return client, nil
	}
	f.client = &http.Client{Transport: m.proxy.Transport, Timeout: 2 * time.Hour}
	return f
}

func (f *forwardWireFixture) close() {
	f.m.Stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.connections {
		_ = c.Close()
	}
}

func (f *forwardWireFixture) post(ctx context.Context, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://primary.test/v1/responses", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Real server requests have no replay body. Forwarding must not add one.
	req.GetBody = nil
	return f.client.Do(req)
}

func forwardTestBody() io.Reader { return strings.NewReader(`{"model":"gpt-test","stream":true}`) }

func requireForwardComplete(t *testing.T, resp *http.Response, err error) string {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(body), forwardCompleted), "missing successful SSE terminal: %q", body)
	return string(body)
}

func TestSiteForwardReplacesDeadConnectionWithoutReplayingPOST(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dead := &forwardPeer{ackDelay: -1, plan: func(int) forwardPeerPlan { return forwardPeerPlan{finishDelay: time.Hour} }}
		good := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{finishDelay: time.Second} }}
		f := newForwardWireFixture(t, dead, good)
		defer f.close()
		resp, err := f.post(t.Context(), forwardTestBody())
		require.Nil(t, resp)
		require.ErrorContains(t, err, "http2: client connection lost")
		require.EqualValues(t, 1, dead.requests.Load(), "failed generation must not be replayed")
		require.EqualValues(t, 0, good.requests.Load())
		resp, err = f.post(t.Context(), forwardTestBody())
		requireForwardComplete(t, resp, err)
		require.EqualValues(t, 2, f.dials.Load())
		require.EqualValues(t, 1, good.requests.Load())
	})
}

func TestSiteForwardAllowsLongThinkingAndLongHealthyStreams(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan forwardPeerPlan
	}{
		{"thinking_before_headers", forwardPeerPlan{finishDelay: 599 * time.Second}},
		{"thinking_after_headers", forwardPeerPlan{headersEarly: true, finishDelay: 599 * time.Second}},
		{"one_hour_stream", forwardPeerPlan{headersEarly: true, finishDelay: time.Hour, heartbeat: 100 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := &forwardPeer{ackDelay: 6 * time.Second, plan: func(int) forwardPeerPlan { return tc.plan }}
				f := newForwardWireFixture(t, peer)
				defer f.close()
				start := time.Now()
				resp, err := f.post(t.Context(), forwardTestBody())
				requireForwardComplete(t, resp, err)
				require.Equal(t, tc.plan.finishDelay, time.Since(start))
				require.EqualValues(t, 1, f.dials.Load())
				require.EqualValues(t, 1, peer.requests.Load())
			})
		})
	}
}

func TestSiteForwardBoundsStalledRequestOnLiveConnection(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "body"}[headers], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := &forwardPeer{plan: func(n int) forwardPeerPlan {
					if n == 1 {
						return forwardPeerPlan{headersEarly: headers, finishDelay: time.Hour}
					}
					return forwardPeerPlan{finishDelay: time.Second}
				}}
				f := newForwardWireFixture(t, peer)
				defer f.close()
				start := time.Now()
				resp, err := f.post(t.Context(), forwardTestBody())
				if headers {
					require.NoError(t, err)
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					require.Equal(t, "data: first\n\n", string(body))
					err = readErr
				}
				var timeout *forwardTimeoutError
				require.ErrorAs(t, err, &timeout)
				require.Equal(t, map[bool]string{false: "response_headers", true: "response_idle"}[headers], timeout.phase)
				require.Equal(t, 600*time.Second, time.Since(start))
				synctest.Wait()
				require.EqualValues(t, 1, peer.requests.Load())
				require.Positive(t, peer.resets.Load(), "cancel the stalled stream on the live connection")
				resp, err = f.post(t.Context(), forwardTestBody())
				requireForwardComplete(t, resp, err)
				require.EqualValues(t, 2, peer.requests.Load(), "only the next explicit request may run")
				// net/http may retire an old now-idle connection. The concurrent
				// sibling test below verifies that active streams are preserved.
				require.LessOrEqual(t, f.dials.Load(), int32(2))
			})
		})
	}
}

func TestSiteForwardStreamTimeoutDoesNotInterruptSibling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := &forwardPeer{plan: func(n int) forwardPeerPlan {
			if n == 1 {
				return forwardPeerPlan{headersEarly: true, finishDelay: time.Hour}
			}
			return forwardPeerPlan{headersEarly: true, heartbeat: 100 * time.Second, finishDelay: 900 * time.Second}
		}}
		f := newForwardWireFixture(t, peer)
		defer f.close()
		bad, err := f.post(t.Context(), forwardTestBody())
		require.NoError(t, err)
		good, err := f.post(t.Context(), forwardTestBody())
		require.NoError(t, err)
		failed := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, bad.Body); _ = bad.Body.Close(); failed <- err }()
		requireForwardComplete(t, good, nil)
		var timeout *forwardTimeoutError
		require.ErrorAs(t, <-failed, &timeout)
		require.Equal(t, "response_idle", timeout.phase)
		require.EqualValues(t, 1, f.dials.Load())
		require.EqualValues(t, 2, peer.requests.Load())
	})
}

func TestSiteForwardPeerResetNeverReplaysGeneration(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_headers", true: "after_output"}[headers], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := &forwardPeer{plan: func(n int) forwardPeerPlan {
					if n == 1 {
						return forwardPeerPlan{headersEarly: headers, reset: true}
					}
					return forwardPeerPlan{finishDelay: time.Second}
				}}
				f := newForwardWireFixture(t, peer)
				defer f.close()
				resp, err := f.post(t.Context(), forwardTestBody())
				if headers {
					require.NoError(t, err)
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					require.NotContains(t, string(body), "response.completed")
					err = readErr
				}
				require.ErrorContains(t, err, "INTERNAL_ERROR; received from peer")
				require.EqualValues(t, 1, peer.requests.Load())
				resp, err = f.post(t.Context(), forwardTestBody())
				requireForwardComplete(t, resp, err)
				require.EqualValues(t, 1, f.dials.Load(), "one RST_STREAM does not poison sibling streams")
			})
		})
	}
}

func TestSiteForwardGOAWAYUsesNewConnectionForNextRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{goAway: true, finishDelay: time.Second} }}
		f := newForwardWireFixture(t, peer)
		defer f.close()
		for i := 0; i < 2; i++ {
			resp, err := f.post(t.Context(), forwardTestBody())
			requireForwardComplete(t, resp, err)
			synctest.Wait()
		}
		require.EqualValues(t, 2, f.dials.Load())
		require.EqualValues(t, 2, peer.requests.Load())
	})
}

func TestSiteForwardUploadIdleTimeoutTracksProgress(t *testing.T) {
	for _, progressing := range []bool{false, true} {
		t.Run(map[bool]string{false: "stalled", true: "slow_but_progressing"}[progressing], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{waitUpload: true, finishDelay: time.Second} }}
				f := newForwardWireFixture(t, peer)
				defer f.close()
				reader, writer := io.Pipe()
				defer func() { _ = writer.Close() }()
				if progressing {
					go func() {
						for i := 0; i < 3; i++ {
							time.Sleep(100 * time.Second)
							if _, err := io.WriteString(writer, "body chunk"); err != nil {
								return
							}
						}
						_ = writer.Close()
					}()
				}
				start := time.Now()
				resp, err := f.post(t.Context(), reader)
				if progressing {
					requireForwardComplete(t, resp, err)
					require.Equal(t, 301*time.Second, time.Since(start))
				} else {
					var timeout *forwardTimeoutError
					require.ErrorAs(t, err, &timeout)
					require.Equal(t, "upload_idle", timeout.phase)
					require.Equal(t, 120*time.Second, time.Since(start))
				}
				require.EqualValues(t, 1, peer.requests.Load())
			})
		})
	}
}

func TestSiteForwardReadBudgetExcludesSlowDownstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{headersEarly: true, finishDelay: time.Second} }}
		f := newForwardWireFixture(t, peer)
		defer f.close()
		resp, err := f.post(t.Context(), forwardTestBody())
		require.NoError(t, err)
		first := make([]byte, len("data: first\n\n"))
		_, err = io.ReadFull(resp.Body, first)
		require.NoError(t, err)
		time.Sleep(1000 * time.Second) // the downstream is not asking us to read
		requireForwardComplete(t, resp, nil)
	})
}

func TestSiteForwardCallerCancellationDoesNotReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{headersEarly: true, finishDelay: time.Hour} }}
		f := newForwardWireFixture(t, peer)
		defer f.close()
		ctx, cancel := context.WithCancel(t.Context())
		resp, err := f.post(ctx, forwardTestBody())
		require.NoError(t, err)
		cancel()
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.ErrorIs(t, err, context.Canceled)
		require.EqualValues(t, 1, peer.requests.Load())
	})
}

func TestSiteForward200StreamsRecoverAfterConnectionBlackhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const count = 200
		dead := &forwardPeer{ackDelay: -1, plan: func(int) forwardPeerPlan { return forwardPeerPlan{headersEarly: true, finishDelay: time.Hour} }}
		good := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{finishDelay: time.Millisecond} }}
		f := newForwardWireFixture(t, dead, good)
		defer f.close()
		failures := make(chan error, count)
		for i := 0; i < count; i++ {
			resp, err := f.post(t.Context(), forwardTestBody())
			require.NoError(t, err)
			go func() { _, err := io.Copy(io.Discard, resp.Body); _ = resp.Body.Close(); failures <- err }()
		}
		for i := 0; i < count; i++ {
			require.ErrorContains(t, <-failures, "http2: client connection lost")
		}
		require.EqualValues(t, count, dead.requests.Load())
		require.EqualValues(t, 0, good.requests.Load(), "never automatically replay the failed batch")
		for i := 0; i < count; i++ {
			resp, err := f.post(t.Context(), forwardTestBody())
			requireForwardComplete(t, resp, err)
		}
		require.EqualValues(t, count, good.requests.Load())
		require.EqualValues(t, 2, f.dials.Load(), "new batch must escape the original failed connection")
	})
}

func TestSiteForwardWebSocketKeepsDuplexBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		kind, body, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(kind, body)
		}
	}))
	defer server.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
	require.NoError(t, err)
	defer m.Stop()
	m.current.Store(&lease{expires: time.Now().Add(ReplicaTTL)})
	trusted, ok := server.Client().Transport.(*http.Transport)
	require.True(t, ok)
	m.forwardTransport.TLSClientConfig.RootCAs = trusted.TLSClientConfig.RootCAs
	router := gin.New()
	router.Use(m.Ingress())
	secondary := httptest.NewServer(router)
	defer secondary.Close()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(secondary.URL, "http")+"/v1/responses", nil)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("duplex roundtrip")))
	kind, body, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, kind)
	require.Equal(t, "duplex roundtrip", string(body))
}

func TestSiteForwardTimeoutConfigCanDisableLongWaitLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer := &forwardPeer{plan: func(int) forwardPeerPlan { return forwardPeerPlan{finishDelay: 1200 * time.Second} }}
		f := newForwardWireFixture(t, peer)
		defer f.close()
		zero := 0
		cfg := Config{ForwardUploadIdleTimeoutSeconds: &zero, ForwardResponseHeaderTimeoutSeconds: &zero, ForwardResponseIdleTimeoutSeconds: &zero}
		require.NoError(t, cfg.Validate())
		guarded, ok := f.m.proxy.Transport.(*forwardRoundTripper)
		require.True(t, ok)
		guarded.timeouts = cfg.forwardTimeouts()
		resp, err := f.post(t.Context(), forwardTestBody())
		requireForwardComplete(t, resp, err)
		for _, n := range []int{-1, 86401} {
			cfg.ForwardResponseHeaderTimeoutSeconds = &n
			require.Error(t, cfg.Validate())
		}
	})
}

func TestSiteForwardHTTP200WithInterruptedBodyIsNotSuccess(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP1", true: "HTTP2"}[h2], func(t *testing.T) {
			var calls atomic.Int32
			cut := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(cut) })
			primary := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				n := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if n > 1 {
					_, _ = io.WriteString(w, forwardCompleted)
					return
				}
				_, _ = io.WriteString(w, "data: first\n\n")
				_ = http.NewResponseController(w).Flush()
				select {
				case <-cut:
					panic(http.ErrAbortHandler)
				case <-r.Context().Done():
					return
				}
			}))
			primary.EnableHTTP2 = h2
			primary.StartTLS()
			defer primary.Close()
			m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: primary.URL, Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
			require.NoError(t, err)
			defer m.Stop()
			m.current.Store(&lease{expires: time.Now().Add(ReplicaTTL)})
			trusted, ok := primary.Client().Transport.(*http.Transport)
			require.True(t, ok)
			m.forwardTransport.TLSClientConfig.RootCAs = trusted.TLSClientConfig.RootCAs
			router := gin.New()
			router.Use(m.Ingress())
			secondary := httptest.NewServer(router)
			defer secondary.Close()
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Post(secondary.URL+"/v1/responses", "application/json", forwardTestBody())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			first := make([]byte, len("data: first\n\n"))
			_, err = io.ReadFull(resp.Body, first)
			require.NoError(t, err)
			once.Do(func() { close(cut) })
			remaining, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.Error(t, err, "200 with a cut stream must remain a transport failure")
			require.NotContains(t, string(remaining), "response.completed")
			require.EqualValues(t, 1, calls.Load(), "must not silently generate a second answer")
			resp, err = client.Post(secondary.URL+"/v1/responses", "application/json", forwardTestBody())
			requireForwardComplete(t, resp, err)
			require.EqualValues(t, 2, calls.Load())
		})
	}
}
