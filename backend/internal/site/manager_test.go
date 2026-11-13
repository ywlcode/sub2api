package site

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type testStore struct {
	snapshot                Snapshot
	events                  []Event
	installed               []SyncResponse
	exportErr, errorInstall error
}

func (s *testStore) MatchLocalModel(key, model, endpoint string) bool {
	return strings.HasPrefix(model, "deepseek-")
}
func (s *testStore) Schema() string { return "test-schema" }
func (s *testStore) Export(context.Context, Peer, []Event, []int64) (Snapshot, error) {
	return s.snapshot, s.exportErr
}
func (s *testStore) PendingIDs(context.Context) ([]int64, error) {
	var ids []int64
	for _, e := range s.events {
		ids = append(ids, e.ID)
	}
	return ids, nil
}
func (s *testStore) Pending(context.Context, int) ([]Event, error) { return s.events, nil }
func (s *testStore) Install(_ context.Context, r SyncResponse) error {
	if s.errorInstall != nil {
		return s.errorInstall
	}
	s.installed = append(s.installed, r)
	return nil
}

func pairedSites(t *testing.T) (*Manager, *testStore, *testStore, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p := Peer{ID: "office", Token: strings.Repeat("x", 32)}
	primaryStore := &testStore{snapshot: Snapshot{Schema: "test-schema", Rows: []Row{{Table: "users", Key: "1", Value: json.RawMessage(`{"id":1,"balance":100.12345678}`)}}}}
	primary, err := New(Config{Mode: Primary, ID: "bosc", Peers: []Peer{p}}, primaryStore, 1<<20)
	require.NoError(t, err)
	r := gin.New()
	r.POST(SyncPath, primary.SyncHandler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	secondaryStore := &testStore{}
	secondary, err := New(Config{Mode: Secondary, ID: p.ID, PrimaryURL: srv.URL, Token: p.Token, AllowHTTP: true}, secondaryStore, 1<<20)
	require.NoError(t, err)
	t.Cleanup(secondary.Stop)
	return secondary, primaryStore, secondaryStore, srv
}

func TestSiteLeaseRenewalDiffFailureAndRestart(t *testing.T) {
	m, primary, local, _ := pairedSites(t)
	now := time.Now()
	m.now = func() time.Time { return now }
	require.False(t, m.Ready())
	require.NoError(t, m.syncOnce(context.Background()))
	require.True(t, m.Ready())
	require.True(t, local.installed[0].Full)
	now = now.Add(5 * time.Second)
	require.NoError(t, m.syncOnce(context.Background()))
	require.False(t, local.installed[1].Full)
	require.Empty(t, local.installed[1].Rows)
	renewedAt := now
	version := m.version
	primary.exportErr = errors.New("primary database down")
	now = now.Add(5 * time.Second)
	require.Error(t, m.syncOnce(context.Background()))
	require.True(t, m.Ready())
	now = renewedAt.Add(time.Minute - time.Nanosecond)
	require.True(t, m.Ready(), "the last successful policy remains valid for one minute")
	require.Error(t, m.syncOnce(context.Background()))
	now = renewedAt.Add(time.Minute)
	require.False(t, m.Ready(), "failed sync must not renew the lease")
	require.Equal(t, version, m.version, "expiry must retain the incremental cursor")
	primary.exportErr = nil
	primary.snapshot.Rows = []Row{{Table: "users", Key: "1", Value: json.RawMessage(`{"id":1,"balance":99.12345678}`)}}
	require.NoError(t, m.syncOnce(context.Background()))
	require.True(t, m.Ready())
	recovered := local.installed[len(local.installed)-1]
	require.False(t, recovered.Full, "recovery after expiry must continue incremental synchronization")
	require.Len(t, recovered.Rows, 1)
	require.JSONEq(t, string(primary.snapshot.Rows[0].Value), string(recovered.Rows[0].Value))
	m.Stop()
	require.False(t, m.Ready(), "persisted snapshot must not grant service on restart")
}

func TestSiteDeletionVersionFallbackAndInstallFailure(t *testing.T) {
	m, primary, local, _ := pairedSites(t)
	require.NoError(t, m.syncOnce(context.Background()))
	primary.snapshot.Rows = nil
	require.NoError(t, m.syncOnce(context.Background()))
	require.Len(t, local.installed[1].Rows, 1)
	require.Empty(t, local.installed[1].Rows[0].Value)
	m.version = "lost-version"
	require.NoError(t, m.syncOnce(context.Background()))
	require.True(t, local.installed[2].Full)
	local.errorInstall = errors.New("disk full")
	require.Error(t, m.syncOnce(context.Background()))
	require.False(t, m.Ready())
	local.errorInstall = nil
	require.NoError(t, m.syncOnce(context.Background()))
	require.True(t, local.installed[len(local.installed)-1].Full, "ambiguous installation must recover with a full snapshot, not get stuck on a stale diff cursor")
	require.True(t, m.Ready())
}

func TestSiteLateSynchronizationDoesNotExtendStaleAuthorization(t *testing.T) {
	m, _, local, _ := pairedSites(t)
	started := time.Now()
	var calls atomic.Int32
	m.now = func() time.Time {
		if calls.Add(1) == 1 {
			return started
		}
		return started.Add(ReplicaTTL + time.Second)
	}
	require.Error(t, m.syncOnce(context.Background()))
	require.Empty(t, local.installed)
	require.False(t, m.Ready())
}

func TestSiteRoutingGzipOriginalCredentialsProtocolsAndExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var primaryCalls, localCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		require.Equal(t, "Bearer original-user-key", r.Header.Get("Authorization"))
		require.Equal(t, "gzip", r.Header.Get("Content-Encoding"))
		gz, err := gzip.NewReader(r.Body)
		require.NoError(t, err)
		defer func() { _ = gz.Close() }()
		raw, err := io.ReadAll(gz)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"model":"gpt-5"`)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		require.NoError(t, http.NewResponseController(w).Flush())
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\"}\n\n")
	}))
	defer primary.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: primary.URL, Token: strings.Repeat("x", 32), AllowHTTP: true}, &testStore{}, 1<<20)
	require.NoError(t, err)
	defer m.Stop()
	now := time.Now()
	m.now = func() time.Time { return now }
	m.current.Store(&lease{expires: now.Add(ReplicaTTL)})
	r := gin.New()
	r.Use(m.Ingress())
	r.POST("/v1/*path", func(c *gin.Context) {
		localCalls++
		require.Equal(t, "Bearer original-user-key", c.GetHeader("Authorization"))
		var reader io.Reader = c.Request.Body
		if c.GetHeader("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(reader)
			require.NoError(t, err)
			defer func() { _ = gz.Close() }()
			reader = gz
		}
		raw, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"reasoning"`)
		require.Contains(t, string(raw), `"call_id":"tool_1"`)
		c.Data(200, "application/json", raw)
	})
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		body := []byte(`{"model":"deepseek-v4","reasoning":{"effort":"high"},"input":[{"type":"function_call_output","call_id":"tool_1","output":"summary"}]}`)
		for _, encoded := range []bool{false, true} {
			wire := body
			if encoded {
				wire, err = compress(body)
				require.NoError(t, err)
			}
			req := httptest.NewRequest("POST", endpoint, bytes.NewReader(wire))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer original-user-key")
			if encoded {
				req.Header.Set("Content-Encoding", "gzip")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			require.JSONEq(t, string(body), w.Body.String())
		}
	}
	require.Equal(t, 6, localCalls)
	require.Zero(t, primaryCalls, "domestic requests must not call the primary")
	body := `{"model":"gpt-5","input":"` + strings.Repeat("a", 2000) + `"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	proxyCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(proxyCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer original-user-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "response.failed", "stream errors must survive HTTP 200 forwarding")
	require.Equal(t, 1, primaryCalls)
	require.Equal(t, 6, localCalls)
	packed, err := compress([]byte(body))
	require.NoError(t, err)
	req = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(packed)).WithContext(proxyCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer original-user-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	require.Equal(t, 2, primaryCalls, "already compressed JSON must retain its encoding")
	now = now.Add(ReplicaTTL)
	for _, model := range []string{"deepseek-v4", "gpt-5"} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+model+`"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, 503, w.Code)
	}
	require.Equal(t, 2, primaryCalls)
	require.Equal(t, 6, localCalls)
}

func TestSiteConfigIsolationAndUnsafeOrigins(t *testing.T) {
	require.NoError(t, (Config{}).Validate())
	for _, origin := range []string{"http://primary.example", "https://user:secret@primary.example", "https://primary.example/path", "https://primary.example?token=leak"} {
		require.Error(t, (Config{Mode: Secondary, ID: "office", PrimaryURL: origin, Token: strings.Repeat("a", 32)}).Validate())
	}
	require.Error(t, (Config{Mode: Standalone, Peers: []Peer{{ID: "bad"}}}).Validate())
	require.NotEqual(t, KeyDigest("first", "sk-client"), KeyDigest("second", "sk-client"))
	require.NoError(t, (Config{Mode: Primary, ID: "main", Peers: []Peer{{ID: "office", Token: strings.Repeat("s", 32)}}}).Validate(), "pairing must not require an account or model list")
}

func TestSiteBodyLimitRejectsWireAndDecodedPayloads(t *testing.T) {
	m, _, _, _ := pairedSites(t)
	m.BodyLimit = func() int64 { return 1024 }
	m.current.Store(&lease{expires: time.Now().Add(ReplicaTTL)})
	r := gin.New()
	r.Use(m.Ingress())
	r.POST("/v1/responses", func(c *gin.Context) {
		t.Error("oversized request reached the local gateway")
	})
	large := []byte(`{"model":"deepseek-v4","input":"` + strings.Repeat("a", 2000) + `"}`)
	packed, err := compress(large)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, encoding string
		body           []byte
		handler        http.Handler
		status         int
	}{
		{"plain", "", large, r, http.StatusRequestEntityTooLarge},
		{"compressed", "gzip", packed, r, http.StatusRequestEntityTooLarge},
		{"explicit local cap", "", large, http.MaxBytesHandler(r, 512), http.StatusRequestEntityTooLarge},
		{"invalid gzip", "gzip", []byte("not gzip"), r, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", tc.encoding)
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code)
		})
	}
}

func TestSiteSyncBatchFitsWithoutLosingEventsOrPendingIDs(t *testing.T) {
	var events []Event
	var ids []int64
	for id := int64(1); id <= 128; id++ {
		events = append(events, Event{ID: id, Kind: "ops_error_logs", Payload: json.RawMessage(`{"detail":"` + strings.Repeat("x", 400) + `"}`)})
		ids = append(ids, id)
	}
	const limit = 2048
	var delivered []Event
	for len(delivered) < len(events) {
		input := SyncRequest{Protocol: ProtocolVersion, NodeID: "office", Events: events[len(delivered):], PendingIDs: ids}
		raw, err := marshalSyncRequest(&input, limit)
		require.NoError(t, err)
		require.LessOrEqual(t, len(raw), limit)
		var decoded SyncRequest
		require.NoError(t, json.Unmarshal(raw, &decoded))
		require.NotEmpty(t, decoded.Events)
		require.Equal(t, ids, decoded.PendingIDs, "older accepted events still need receipt lookup")
		delivered = append(delivered, decoded.Events...)
	}
	require.Equal(t, events, delivered)
	_, err := marshalSyncRequest(&SyncRequest{PendingIDs: ids}, 16)
	require.ErrorContains(t, err, "metadata")
	_, err = marshalSyncRequest(&SyncRequest{Events: events[:1]}, 256)
	require.ErrorContains(t, err, "event 1")
}

func TestSitePrimaryAndStandaloneAlwaysKeepDirectGatewayRequests(t *testing.T) {
	for _, mode := range []string{Primary, Standalone} {
		m, err := New(Config{Mode: mode, ID: "main"}, &testStore{}, 0)
		require.NoError(t, err)
		r := gin.New()
		r.Use(m.Ingress())
		r.POST("/v1/responses", func(c *gin.Context) {
			body, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			c.Data(200, "application/json", body)
		})
		for _, model := range []string{"deepseek-flash", "gpt-5", "custom-model"} {
			body := `{"model":"` + model + `","input":"direct request"}`
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			require.Equal(t, body, w.Body.String())
		}
	}
}

func TestSitePeerAuthenticationAndIdentityBinding(t *testing.T) {
	m, _, local, server := pairedSites(t)
	input := SyncRequest{Protocol: ProtocolVersion, NodeID: "office", Schema: local.Schema()}
	for _, tc := range []struct {
		name, token, node string
		status            int
	}{
		{"missing", "", "office", 401},
		{"bare token", strings.Repeat("x", 32), "office", 401},
		{"wrong token", "Bearer " + strings.Repeat("y", 32), "office", 401},
		{"another node", "Bearer " + strings.Repeat("x", 32), "another", 400},
		{"paired", "Bearer " + strings.Repeat("x", 32), "office", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input.NodeID = tc.node
			raw, err := json.Marshal(input)
			require.NoError(t, err)
			req, err := http.NewRequest("POST", server.URL+SyncPath, bytes.NewReader(raw))
			require.NoError(t, err)
			req.Header.Set("Authorization", tc.token)
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, tc.status, resp.StatusCode)
		})
	}
	require.NoError(t, m.syncOnce(context.Background()))
	m.primaryID = "different-primary"
	require.ErrorContains(t, m.syncOnce(context.Background()), "identity changed")
	require.Len(t, local.installed, 1)
}

func TestSiteVerifiesTLSAndDoesNotRedirectCredentials(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer target.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: target.URL, Token: strings.Repeat("x", 32)}, &testStore{}, 1<<20)
	require.NoError(t, err)
	defer m.Stop()
	require.Error(t, m.syncOnce(context.Background()), "unknown CA must be rejected")
	require.Zero(t, calls.Load())
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+SyncPath, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	m.cfg.PrimaryURL = redirect.URL
	require.ErrorContains(t, m.syncOnce(context.Background()), "status 307")
	require.Zero(t, calls.Load(), "pairing token must not follow redirects")
}

func TestSiteStandaloneAndSecondaryDoNotExposeControlPlaneOrPanel(t *testing.T) {
	standalone, err := New(Config{}, nil, 0)
	require.NoError(t, err)
	r := gin.New()
	r.POST(SyncPath, standalone.SyncHandler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", SyncPath, nil))
	require.Equal(t, 404, w.Code)
	m, _, _, _ := pairedSites(t)
	r = gin.New()
	r.Use(m.Ingress())
	r.Any("/*path", func(c *gin.Context) { c.Status(200) })
	for _, path := range []string{"/api/v1/admin/users", "/api/v1/auth/login", "/", SyncPath} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		require.Equal(t, 404, w.Code, path)
	}
}

func TestSiteProxyFlushesAndCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		require.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: upstream.URL, Token: strings.Repeat("x", 32), AllowHTTP: true}, &testStore{}, 1<<20)
	require.NoError(t, err)
	defer m.Stop()
	m.current.Store(&lease{expires: time.Now().Add(ReplicaTTL)})
	r := gin.New()
	r.Use(m.Ingress())
	r.POST("/v1/responses", func(*gin.Context) { t.Error("foreign request reached local handler") })
	proxy := httptest.NewServer(r)
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-test"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := proxy.Client().Do(req)
	require.NoError(t, err)
	buf := make([]byte, len("data: first\n\n"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err, "first event must arrive while upstream remains open")
	require.Equal(t, "data: first\n\n", string(buf))
	_ = resp.Body.Close()
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel upstream")
	}
}

func TestSiteSyncStatsFullDeltaAndHeartbeat(t *testing.T) {
	m, primary, _, _ := pairedSites(t)
	require.NoError(t, m.syncOnce(t.Context()))
	full, ok := m.LastSync()
	require.True(t, ok)
	require.True(t, full.Full)
	require.Positive(t, full.UploadWireBytes)
	require.Positive(t, full.DownloadWireBytes)
	require.NoError(t, m.syncOnce(t.Context()))
	idle, _ := m.LastSync()
	require.False(t, idle.Full)
	require.Zero(t, idle.Rows)
	require.Less(t, idle.DownloadWireBytes, full.DownloadWireBytes)
	_ = primary
}
