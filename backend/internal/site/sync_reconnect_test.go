package site

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSiteSyncTimeoutDiscardsStalledConnection(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			var mu sync.Mutex
			var stalled string
			connections := make(chan string, 4)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				if stalled == "" {
					stalled = r.RemoteAddr
				}
				blocked := r.RemoteAddr == stalled
				mu.Unlock()
				connections <- r.RemoteAddr
				if blocked {
					if phase == "body" {
						_, _ = io.WriteString(w, `{"protocol":`)
						_ = http.NewResponseController(w).Flush()
					}
					<-r.Context().Done()
					return
				}
				c, _ := gin.CreateTestContext(w)
				encodeSiteResponse(c, SyncResponse{Protocol: ProtocolVersion, NodeID: "office", PrimaryID: "main", Version: "v1", Full: true})
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
			require.NoError(t, err)
			defer m.Stop()
			syncTransport, ok := m.syncClients[0].Transport.(*http.Transport)
			require.True(t, ok)
			serverTransport, ok := server.Client().Transport.(*http.Transport)
			require.True(t, ok)
			syncTransport.TLSClientConfig.RootCAs = serverTransport.TLSClientConfig.RootCAs
			input := SyncRequest{Protocol: ProtocolVersion, NodeID: "office"}
			var output SyncResponse
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			_, err = m.postSite(ctx, m.syncClients[0], SyncPath, &input, &output)
			cancel()
			require.ErrorIs(t, err, context.DeadlineExceeded)
			retryCtx, cancelRetry := context.WithTimeout(t.Context(), time.Second)
			defer cancelRetry()
			_, err = m.postSite(retryCtx, m.syncClients[0], SyncPath, &input, &output)
			require.NoError(t, err, "a fresh sync must escape the stalled TCP connection")
			require.NotEqual(t, <-connections, <-connections)
		})
	}
}

func TestSiteSyncUsesFiveWarmPoolsAndPreservesLeaseRules(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		for _, failedPools := range []int{1, syncConnectionPools - 1} {
			t.Run(fmt.Sprintf("%s/failed_pools_%d", phase, failedPools), func(t *testing.T) {
				var broken atomic.Pointer[map[string]bool]
				var allBroken atomic.Bool
				connections := make(chan string, 32)
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					connections <- r.RemoteAddr
					bad := broken.Load()
					if allBroken.Load() || bad != nil && (*bad)[r.RemoteAddr] {
						if phase == "body" {
							_, _ = io.WriteString(w, `{"primary_id":"partial-invalid-history","version":"poison"`)
							_ = http.NewResponseController(w).Flush()
						}
						<-r.Context().Done()
						return
					}
					c, _ := gin.CreateTestContext(w)
					encodeSiteResponse(c, SyncResponse{Protocol: ProtocolVersion, NodeID: "office", PrimaryID: "main", Version: "v1", Schema: "test-schema", Full: true})
				}))
				server.EnableHTTP2 = true
				server.StartTLS()
				defer server.Close()
				local := &deliveryTestStore{}
				m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, Token: strings.Repeat("s", 32)}, local, 1<<20)
				require.NoError(t, err)
				defer m.Stop()
				serverTransport, ok := server.Client().Transport.(*http.Transport)
				require.True(t, ok)
				for _, client := range m.syncClients {
					transport, ok := client.Transport.(*http.Transport)
					require.True(t, ok)
					transport.TLSClientConfig.RootCAs = serverTransport.TLSClientConfig.RootCAs
					client.Timeout = 200 * time.Millisecond
				}
				now := time.Now()
				m.now = func() time.Time { return now }
				warm := make([]string, 0, syncConnectionPools)
				bad := make(map[string]bool)
				for i := 0; i < syncConnectionPools; i++ {
					require.NoError(t, m.syncOnce(t.Context()))
					address := <-connections
					require.NotContains(t, warm, address)
					warm = append(warm, address)
					if i < failedPools {
						bad[address] = true
					}
				}
				broken.Store(&bad)
				expires := m.current.Load().expires
				now = now.Add(SyncInterval)
				// At most two attempts per round, continuing through all five pools.
				// Advancing the fake lease clock uses production timeout budgets;
				// only the network fixture deadlines are shortened for this test.
				for tried := 0; tried < failedPools+1; {
					err = m.syncOnce(t.Context())
					attempts := min(syncAttemptsPerRound, failedPools+1-tried)
					for i := 0; i < attempts; i++ {
						require.Equal(t, warm[tried+i], <-connections, "retries must advance to an independent warm pool")
					}
					tried += attempts
					if tried <= failedPools {
						require.Error(t, err)
						require.Equal(t, expires, m.current.Load().expires)
						require.Equal(t, syncConnectionPools, local.applied)
						now = now.Add(syncAttemptsPerRound * syncAttemptTimeout)
					} else {
						require.NoError(t, err)
					}
				}
				require.True(t, now.Before(expires), "four failed connections must still leave time for the fifth")
				require.Equal(t, "main", m.primaryID)
				require.Equal(t, syncConnectionPools+1, local.applied, "failed and partial snapshots must never be installed")
				require.True(t, m.Ready())

				allBroken.Store(true)
				expires = m.current.Load().expires
				require.Error(t, m.syncOnce(t.Context()))
				require.Equal(t, expires, m.current.Load().expires, "failure must not extend the lease")
				require.Equal(t, syncConnectionPools+1, local.applied)
				now = now.Add(ReplicaTTL + time.Second)
				require.False(t, m.Ready())
				allBroken.Store(false)
				require.NoError(t, m.syncOnce(t.Context()), "reconnect must recover without restart or clearing local state")
				require.True(t, m.Ready())
			})
		}
	}
}

func TestSiteSyncRetriesTransientStatusButNotAuthOrInvalidData(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "invalid snapshot")
					return
				}
				c, _ := gin.CreateTestContext(w)
				encodeSiteResponse(c, SyncResponse{Protocol: ProtocolVersion, NodeID: "office", PrimaryID: "main", Version: "v1", Schema: "test-schema", Full: true})
			}))
			defer server.Close()
			m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, AllowHTTP: true, Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
			require.NoError(t, err)
			defer m.Stop()
			err = m.syncOnce(t.Context())
			if status == http.StatusServiceUnavailable {
				require.NoError(t, err)
				require.EqualValues(t, 2, calls.Load())
				require.True(t, m.Ready())
			} else {
				require.Error(t, err)
				require.EqualValues(t, 1, calls.Load())
				require.False(t, m.Ready())
			}
		})
	}
}
