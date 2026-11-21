package site

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type discoveryTestStore struct{ testStore }

func (*discoveryTestStore) HasDiscoveryKey(key string) bool { return key == "known" }

func TestDiscoveryAdmissionFallbackAndLease(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "primary")
		_, _ = io.WriteString(w, r.Header.Get("Authorization")+"|"+r.URL.RawQuery+"|"+r.Header.Get("If-None-Match"))
	}))
	defer upstream.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", Token: strings.Repeat("a", 32), PrimaryURL: upstream.URL, AllowHTTP: true}, &discoveryTestStore{}, 0)
	require.NoError(t, err)
	t.Cleanup(m.Stop)
	now := time.Now()
	m.now = func() time.Time { return now }
	d := &catalogDownload{}
	d.ready.Store(&catalogContents{groups: map[int64]CatalogGroup{1: {Plain: []byte("local")}}})
	m.current.Store(&lease{expires: now.Add(ReplicaTTL), catalog: &catalogSnapshot{download: d}})
	router := gin.New()
	router.Use(m.Ingress())
	router.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Deny") != "" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		if c.GetHeader("X-Test-Expire") != "" {
			now = now.Add(ReplicaTTL)
		}
		c.Next()
	})
	router.GET("/v1/models", func(c *gin.Context) {
		lookup := DiscoveryFrom(c)
		require.NotNil(t, lookup)
		// A gateway middleware can decorate its local request. The primary
		// fallback must still receive the original caller and variant.
		c.Request.Header.Del("Authorization")
		c.Request.URL.RawQuery = ""
		if body := lookup(1, c.GetHeader("X-Test-Kind")); body != nil {
			c.Data(200, "text/plain", body)
			return
		}
		ForwardDiscovery(c)
	})
	check := func(credential, kind, action string, status int, body, servedBy string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/models?client_version=1", nil)
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		req = req.WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("If-None-Match", `"previous"`)
		req.Header.Set("X-Test-Kind", kind)
		if action != "" {
			req.Header.Set(action, "yes")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, status, w.Code)
		require.Contains(t, w.Body.String(), body)
		require.Equal(t, servedBy, w.Header().Get("X-Served-By"))
	}
	check("known", CatalogPlain, "", 200, "local", "")
	check("unknown", CatalogPlain, "", 200, `Bearer unknown|client_version=1|"previous"`, "primary")
	check("known", CatalogCodex, "", 200, `Bearer known|client_version=1|"previous"`, "primary")
	check("known", CatalogPlain, "X-Test-Deny", 403, "", "")
	check("known", CatalogCodex, "X-Test-Expire", 503, "replica_expired", "")
	check("unknown", CatalogPlain, "", 503, "replica_expired", "")
}

func TestDiscoveryMatchesRegisteredCollectionsOnly(t *testing.T) {
	router := gin.New()
	h := func(c *gin.Context) {
		if discoveryCollection(c) {
			c.Status(204)
		} else {
			c.Status(202)
		}
	}
	for _, path := range []string{"/models", "/v1/models", "/models/:model", "/v1/models/:model", "/backend-api/codex/models", "/v1beta/models", "/v1/responses"} {
		router.GET(path, h)
	}
	router.POST("/v1/models", h)
	for _, tc := range []struct {
		method, path, upgrade string
		want                  int
	}{
		{"GET", "/models", "", 204}, {"GET", "/v1/models?client_version=0.153.0", "", 204},
		{"GET", "/backend-api/codex/models?client_version=1", "", 204}, {"GET", "/models/gpt", "", 202},
		{"GET", "/v1/models/gpt?client_version=1", "", 202}, {"GET", "/models?model=", "", 202},
		{"GET", "/v1/models?future_option=1", "", 202}, {"GET", "/v1beta/models", "", 202},
		{"GET", "/models?client_version=%zz", "", 202},
		{"GET", "/v1/responses", "websocket", 202}, {"GET", "/v1/models", "websocket", 202},
		{"POST", "/v1/models", "", 202},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Upgrade", tc.upgrade)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, tc.want, w.Code, "%s %s %s", tc.method, tc.path, tc.upgrade)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	require.Nil(t, DiscoveryFrom(c))
	ForwardDiscovery(c)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}
