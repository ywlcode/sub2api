package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type catalogMemoryStore struct{}

func (catalogMemoryStore) Schema() string                                     { return "handler-test" }
func (catalogMemoryStore) Pending(context.Context, int) ([]site.Event, error) { return nil, nil }
func (catalogMemoryStore) PendingIDs(context.Context) ([]int64, error)        { return nil, nil }
func (catalogMemoryStore) Install(context.Context, site.SyncResponse) error   { return nil }
func (catalogMemoryStore) MatchLocalModel(string, string, string) bool        { return false }
func (catalogMemoryStore) HasDiscoveryKey(string) bool                        { return true }
func (s catalogMemoryStore) Export(context.Context, site.Peer, []site.Event, []int64) (site.Snapshot, error) {
	return site.Snapshot{Schema: s.Schema()}, nil
}

type catalogSlotCounter struct {
	service.ConcurrencyCache
	service.APIKeySlotLeaseCache
	attempts atomic.Int32
}

func (*catalogSlotCounter) APIKeySlotTTL() time.Duration             { return time.Minute }
func (*catalogSlotCounter) APIKeySlotRefreshInterval() time.Duration { return 10 * time.Second }
func (s *catalogSlotCounter) AcquireAPIKeySlot(context.Context, int64, int, string) (bool, error) {
	s.attempts.Add(1)
	return true, nil
}
func (s *catalogSlotCounter) TrackAPIKeySlot(context.Context, int64, string) error {
	s.attempts.Add(1)
	return nil
}
func (*catalogSlotCounter) ReleaseAPIKeySlot(context.Context, int64, string) error { return nil }

// Keep the ingress index eligible while changing the authenticated key. This
// exercises the handler-side guard across a configuration install, including
// the real site's capture/replay of request headers and raw query bytes.
func TestSiteCatalogForwardsOriginalRequestBeforeSecondarySlot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	peer := site.Peer{ID: "secondary", Token: strings.Repeat("t", 32)}
	primary, err := site.New(site.Config{Mode: site.Primary, ID: "primary", Peers: []site.Peer{peer}}, catalogMemoryStore{}, 0)
	require.NoError(t, err)
	t.Cleanup(primary.Stop)
	group := &service.Group{ID: 41, Platform: service.PlatformGrok}
	const ordinary = `{"data":[{"id":"group-visible"}],"object":"list"}`
	primary.LoadCatalog = func(context.Context) (map[int64]site.CatalogGroup, error) {
		return map[int64]site.CatalogGroup{group.ID: {
			Policy: service.SiteCatalogGroupPolicy(group), Plain: []byte(ordinary), Union: []byte("[]"), Codex: []byte(`{"models":[]}`),
		}}, nil
	}
	mainRoutes := gin.New()
	mainRoutes.POST(site.SyncPath, primary.SyncHandler)
	mainRoutes.GET("/models", func(c *gin.Context) {
		c.Header("ETag", `"from-primary"`)
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusAccepted, "%s %s %s %s", c.Request.Method, c.Request.URL.RequestURI(), c.GetHeader("Authorization"), c.GetHeader("If-None-Match"))
	})
	server := httptest.NewServer(mainRoutes)
	t.Cleanup(server.Close)
	secondary, err := site.New(site.Config{Mode: site.Secondary, ID: peer.ID, Token: peer.Token, PrimaryURL: server.URL, AllowHTTP: true}, catalogMemoryStore{}, 0)
	require.NoError(t, err)
	t.Cleanup(secondary.Stop)

	slots := &catalogSlotCounter{}
	h := &GatewayHandler{concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(slots), SSEPingFormatNone, time.Second)}
	var intercepted atomic.Int32
	localRoutes := gin.New()
	localRoutes.Use(secondary.Ingress())
	localRoutes.GET("/models", func(c *gin.Context) {
		current := *group
		key := catalogKeyForTest(&current)
		switch c.GetHeader("X-Test-Key-State") {
		case "slot":
			key.ConcurrencyLimit = 1
		case "allow-IP":
			key.IPWhitelist = []string{"192.0.2.1"}
		case "deny-IP":
			key.IPBlacklist = []string{"192.0.2.2"}
		case "new-policy":
			current.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"other"}}
		case "forced":
			c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformGemini)
		case "resolved":
			c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenAI))
		case "no-candidates":
			key.GroupIDs = []int64{current.ID, 42}
		}
		ctx, cancel := service.WithAPIKeyAdmissionOwner(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Set(string(middleware.ContextKeyAPIKey), key)
		if h.ServeSiteCatalog(c) {
			intercepted.Add(1)
			return
		}
		c.Status(http.StatusNotFound)
	})
	request := func(path, state string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer synthetic-test-key")
		req.Header.Set("If-None-Match", `W/"original"`)
		req.Header.Set("X-Test-Key-State", state)
		w := httptest.NewRecorder()
		localRoutes.ServeHTTP(w, req)
		return w
	}

	secondary.Start()
	require.Eventually(t, func() bool {
		w := request("/models", "")
		return w.Code == http.StatusOK && w.Body.String() == ordinary && w.Result().Header.Get("Cache-Control") == "private, no-cache"
	}, 3*site.SyncInterval, 100*time.Millisecond, "wait for the site to install the eligible catalog")
	for _, state := range []string{"slot", "allow-IP", "deny-IP", "new-policy", "forced", "resolved", "no-candidates"} {
		t.Run(state, func(t *testing.T) {
			path := "/models?client_version=%30.1"
			if state == "no-candidates" {
				path = "/models?client_version="
			}
			previous := intercepted.Load()
			w := request(path, state)
			require.Equal(t, previous+1, intercepted.Load(), "fallback must pass through ServeSiteCatalog")
			require.Equal(t, http.StatusAccepted, w.Code)
			require.Equal(t, `"from-primary"`, w.Header().Get("ETag"))
			require.Equal(t, []string{"no-store"}, w.Result().Header.Values("Cache-Control"))
			require.Equal(t, "GET "+path+` Bearer synthetic-test-key W/"original"`, w.Body.String())
			require.Zero(t, slots.attempts.Load())
		})
	}
}
