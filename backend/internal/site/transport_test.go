package site

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type sitePoolRequest struct {
	path, connection string
	protocol         int
}

// Keep the first connection unusable even after its request is canceled. A
// retry must use another TCP connection, rather than another stream on the same
// HTTP/2 session. Exercise both missing headers and a partially received receipt.
func TestSiteDeliveryTimeoutDiscardsStalledConnection(t *testing.T) {
	for _, phase := range []string{"headers", "receipt_body"} {
		t.Run(phase, func(t *testing.T) {
			var mu sync.Mutex
			var stalled string
			var received []SyncRequest
			connections := make(chan string, 4)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				var input SyncRequest
				if err := decodeSiteRequest(c, &input); err != nil {
					t.Errorf("decode event upload: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				if stalled == "" {
					stalled = r.RemoteAddr
				}
				blocked := r.RemoteAddr == stalled
				received = append(received, input)
				mu.Unlock()
				connections <- r.RemoteAddr
				if blocked {
					if phase == "receipt_body" {
						_, _ = io.WriteString(w, `{"protocol":`)
						_ = http.NewResponseController(w).Flush()
					}
					<-r.Context().Done()
					return
				}
				out := DeliveryResponse{Protocol: ProtocolVersion, NodeID: input.NodeID}
				for _, e := range input.Events {
					out.Acknowledged = append(out.Acknowledged, e.ID)
				}
				_ = json.NewEncoder(w).Encode(out)
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			local := &deliveryTestStore{pending: []Event{{ID: 7, Kind: "billing", Payload: json.RawMessage(`{"amount":0.125}`)}}}
			manager, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, Token: strings.Repeat("p", 32)}, local, 1<<20)
			require.NoError(t, err)
			defer manager.Stop()
			transport, ok := manager.deliveryClient.Transport.(*http.Transport)
			require.True(t, ok)
			serverTransport, ok := server.Client().Transport.(*http.Transport)
			require.True(t, ok)
			transport.TLSClientConfig.RootCAs = serverTransport.TLSClientConfig.RootCAs
			transport.MaxConnsPerHost = 1
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			_, err = manager.deliverOnce(ctx)
			cancel()
			require.ErrorIs(t, err, context.DeadlineExceeded)
			pending, err := local.Pending(t.Context(), DeliveryBatch)
			require.NoError(t, err)
			require.Len(t, pending, 1, "lost acknowledgement must preserve the outbox")
			retryCtx, cancelRetry := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancelRetry()
			active, err := manager.deliverOnce(retryCtx)
			require.NoError(t, err, "retry must escape the failed connection")
			require.True(t, active)
			require.NotEqual(t, <-connections, <-connections)
			mu.Lock()
			attempts := append([]SyncRequest(nil), received...)
			mu.Unlock()
			require.Len(t, attempts, 2)
			require.Equal(t, attempts[0], attempts[1], "reconnect must preserve original event identity and payload")
			pending, err = local.Pending(t.Context(), DeliveryBatch)
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}
}

type sitePoolFixture struct {
	manager       *Manager
	local, remote *deliveryTestStore
	requests      chan sitePoolRequest
	closed        chan string
}

func newSitePoolFixture(t *testing.T, http2 bool, model http.HandlerFunc) *sitePoolFixture {
	t.Helper()
	f := &sitePoolFixture{
		local:    &deliveryTestStore{},
		remote:   &deliveryTestStore{wait: make(chan struct{}), started: make(chan struct{}, 4)},
		requests: make(chan sitePoolRequest, 1024),
		closed:   make(chan string, 1024),
	}
	token := strings.Repeat("p", 32)
	primary, err := New(Config{Mode: Primary, ID: "main", Peers: []Peer{{ID: "office", Token: token}}}, f.remote, 1<<20)
	require.NoError(t, err)
	t.Cleanup(primary.Stop)
	router := gin.New()
	router.POST(SyncPath, primary.SyncHandler)
	router.POST(DeliveryPath, primary.DeliveryHandler)
	router.POST("/v1/responses", gin.WrapH(model))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests <- sitePoolRequest{path: r.URL.Path, connection: r.RemoteAddr, protocol: r.ProtoMajor}
		router.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = http2
	server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			f.closed <- c.RemoteAddr().String()
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	f.manager, err = New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, Token: token}, f.local, 1<<20)
	require.NoError(t, err)
	t.Cleanup(f.manager.Stop)
	serverTransport, ok := server.Client().Transport.(*http.Transport)
	require.True(t, ok)
	trust := serverTransport.TLSClientConfig
	deliveryTransport, ok := f.manager.deliveryClient.Transport.(*http.Transport)
	require.True(t, ok)
	transports := []*http.Transport{deliveryTransport, f.manager.forwardTransport}
	for _, client := range f.manager.syncClients {
		transport, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		transports = append(transports, transport)
	}
	for _, transport := range transports {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		// Keep the transport's ALPN settings while trusting the test server.
		transport.TLSClientConfig.RootCAs = trust.RootCAs
		// A busy connection must not exhaust another traffic class's pool.
		transport.MaxConnsPerHost = 1
	}
	return f
}

func TestSiteSyncAndBillingRemainIndependentWith200ModelStreams(t *testing.T) {
	const count = 200
	started := make(chan struct{}, count)
	release := make(chan struct{})
	var releaseOnce sync.Once
	f := newSitePoolFixture(t, true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()
		started <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
		case <-r.Context().Done():
		}
	})
	close(f.remote.wait)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, f.manager.syncOnce(ctx))
	router := gin.New()
	router.Use(f.manager.Ingress())
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusTeapot) })
	secondary := httptest.NewServer(router)
	t.Cleanup(secondary.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	results := make(chan string, count)
	for i := 0; i < count; i++ {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, secondary.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true}`))
			if err != nil {
				results <- err.Error()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := secondary.Client().Do(req)
			if err != nil {
				results <- err.Error()
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				results <- err.Error()
			} else if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.completed") {
				results <- "missing successful stream terminal event"
			} else {
				results <- ""
			}
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("model streams did not all start")
		}
	}
	for i := 0; i < 10; i++ {
		require.NoError(t, f.manager.syncOnce(ctx))
		require.True(t, f.manager.Ready())
		f.queueEvent(int64(i + 1))
		active, err := f.manager.deliverOnce(ctx)
		require.NoError(t, err)
		require.True(t, active)
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < count; i++ {
		select {
		case result := <-results:
			require.Empty(t, result)
		case <-ctx.Done():
			t.Fatal("model streams did not finish")
		}
	}
}

func (f *sitePoolFixture) queueEvent(id int64) {
	f.local.mu.Lock()
	defer f.local.mu.Unlock()
	f.local.pending = []Event{{ID: id, Kind: "billing", Payload: json.RawMessage(`{"amount":0.125}`)}}
}

func TestSitePoolsReuseOnlyWithinTrafficClassAndCloseOnStop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []struct {
		name  string
		major int
	}{{"HTTP1", 1}, {"HTTP2", 2}} {
		t.Run(protocol.name, func(t *testing.T) {
			f := newSitePoolFixture(t, protocol.major == 2, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
			})
			close(f.remote.wait)
			router := gin.New()
			router.Use(f.manager.Ingress())
			router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusTeapot) })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			connections := make(map[string]string)
			for i := 0; i < 2*syncConnectionPools; i++ {
				require.NoError(t, f.manager.syncOnce(ctx))
				f.queueEvent(int64(i + 1))
				active, err := f.manager.deliverOnce(ctx)
				require.NoError(t, err)
				require.True(t, active)
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true}`)).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code)
				require.Contains(t, response.Body.String(), "response.completed")
				for j := 0; j < 3; j++ {
					select {
					case observed := <-f.requests:
						wantProtocol := protocol.major
						if observed.path == DeliveryPath || observed.path == SyncPath {
							wantProtocol = 1
						}
						require.Equal(t, wantProtocol, observed.protocol)
						if observed.path == SyncPath {
							observed.path += "/pool/" + strconv.Itoa(i%syncConnectionPools)
						}
						if previous, ok := connections[observed.path]; ok {
							require.Equal(t, previous, observed.connection, "each traffic class should reuse its connection")
						}
						connections[observed.path] = observed.connection
					case <-ctx.Done():
						t.Fatal("missing primary request")
					}
				}
			}
			require.Len(t, connections, syncConnectionPools+2)
			unique := make(map[string]bool)
			for _, connection := range connections {
				unique[connection] = true
			}
			require.Len(t, unique, len(connections), "all sync pools, billing and model forwarding must own separate connections")
			f.manager.Stop()
			require.False(t, f.manager.Ready())
			closed := make(map[string]bool)
			for len(closed) < len(connections) {
				select {
				case address := <-f.closed:
					closed[address] = true
				case <-ctx.Done():
					t.Fatal("Stop did not release every idle connection")
				}
			}
			for _, connection := range connections {
				require.True(t, closed[connection])
			}
		})
	}
}

func TestSitePoolsStalledModelAndDeliveryDoNotBlockSync(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, http2 := range []bool{false, true} {
		name := "HTTP1"
		if http2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			modelCanceled := make(chan struct{})
			f := newSitePoolFixture(t, http2, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: first\n\n")
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
				close(modelCanceled)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, f.manager.syncOnce(ctx))
			router := gin.New()
			router.Use(f.manager.Ingress())
			router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusTeapot) })
			secondary := httptest.NewServer(router)
			defer secondary.Close()
			modelCtx, cancelModel := context.WithCancel(ctx)
			defer cancelModel()
			request, err := http.NewRequestWithContext(modelCtx, http.MethodPost, secondary.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true}`))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			response, err := secondary.Client().Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			first := make([]byte, len("data: first\n\n"))
			_, err = io.ReadFull(response.Body, first)
			require.NoError(t, err)
			require.Equal(t, "data: first\n\n", string(first))

			f.queueEvent(1)
			uploadCtx, cancelUpload := context.WithCancel(ctx)
			defer cancelUpload()
			uploadDone := make(chan error, 1)
			go func() {
				_, err := f.manager.deliverOnce(uploadCtx)
				uploadDone <- err
			}()
			select {
			case <-f.remote.started:
			case <-ctx.Done():
				t.Fatal("billing upload did not start while model stream was open")
			}
			// Both other pools have an occupied connection. Lease renewal still
			// succeeds, even with one available connection per pool.
			for i := 0; i < 3; i++ {
				require.NoError(t, f.manager.syncOnce(ctx))
				require.True(t, f.manager.Ready())
			}
			cancelUpload()
			select {
			case err := <-uploadDone:
				require.ErrorIs(t, err, context.Canceled)
			case <-ctx.Done():
				t.Fatal("billing cancellation did not finish")
			}
			pending, err := f.local.Pending(ctx, DeliveryBatch)
			require.NoError(t, err)
			require.Len(t, pending, 1, "a lost delivery response must not acknowledge billing")
			close(f.remote.wait)
			active, err := f.manager.deliverOnce(ctx)
			require.NoError(t, err)
			require.True(t, active)
			pending, err = f.local.Pending(ctx, DeliveryBatch)
			require.NoError(t, err)
			require.Empty(t, pending)
			require.NoError(t, f.manager.syncOnce(ctx))
			cancelModel()
			select {
			case <-modelCanceled:
			case <-ctx.Done():
				t.Fatal("client cancellation did not reach the primary model request")
			}
		})
	}
}
