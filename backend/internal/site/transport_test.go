package site

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type sitePoolRequest struct {
	path, connection string
	protocol         int
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
		requests: make(chan sitePoolRequest, 32),
		closed:   make(chan string, 32),
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
	syncTransport, ok := f.manager.syncClient.Transport.(*http.Transport)
	require.True(t, ok)
	deliveryTransport, ok := f.manager.deliveryClient.Transport.(*http.Transport)
	require.True(t, ok)
	for _, transport := range []*http.Transport{
		syncTransport,
		deliveryTransport,
		f.manager.forwardTransport,
	} {
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
			for i := 0; i < 2; i++ {
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
						require.Equal(t, protocol.major, observed.protocol)
						if previous, ok := connections[observed.path]; ok {
							require.Equal(t, previous, observed.connection, "each traffic class should reuse its connection")
						}
						connections[observed.path] = observed.connection
					case <-ctx.Done():
						t.Fatal("missing primary request")
					}
				}
			}
			require.Len(t, connections, 3)
			require.NotEqual(t, connections[SyncPath], connections[DeliveryPath], "policy and billing must use separate connections")
			require.NotEqual(t, connections[SyncPath], connections["/v1/responses"], "policy and models must use separate connections")
			require.NotEqual(t, connections[DeliveryPath], connections["/v1/responses"], "billing and models must use separate connections")
			f.manager.Stop()
			require.False(t, f.manager.Ready())
			closed := make(map[string]bool)
			for len(closed) < len(connections) {
				select {
				case address := <-f.closed:
					closed[address] = true
				case <-ctx.Done():
					t.Fatal("Stop did not release all three idle connections")
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
