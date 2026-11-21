package site

import (
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

// The request carries a lookup, never a pre-authorized response. The normal
// gateway middleware must first identify the user and their eligible groups.
const discoveryContextKey = "site.authenticated_discovery"

type discoveryRequest struct {
	lookup  func(int64, string) []byte
	forward func(*gin.Context)
}

func DiscoveryFrom(c *gin.Context) func(int64, string) []byte {
	v, _ := c.Get(discoveryContextKey)
	if d, ok := v.(discoveryRequest); ok {
		return d.lookup
	}
	return nil
}

// ForwardDiscovery replays the unmodified request through the usual primary
// transport (including its egress policy). Authentication failures do not call
// this function; they terminate in the normal middleware.
func ForwardDiscovery(c *gin.Context) {
	v, _ := c.Get(discoveryContextKey)
	if d, ok := v.(discoveryRequest); ok {
		d.forward(c)
		return
	}
	unavailable(c)
}

func discoveryCollection(c *gin.Context) bool {
	// Check the registered route as well as the wire path; a future wildcard
	// handler must not silently inherit the admission rules of a collection.
	return c.FullPath() == c.Request.URL.Path && DiscoveryKind(c.Request) != ""
}

// DiscoveryKind identifies exactly the request representations synchronized
// by the primary. Unrecognized selectors, protocols or methods stay there.
func DiscoveryKind(r *http.Request) string {
	if r.Method != http.MethodGet || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.Header.Get("Upgrade") != "" {
		return ""
	}
	switch r.URL.Path {
	case "/models", "/v1/models", "/backend-api/codex/models":
	default:
		return ""
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return ""
	}
	for name := range query {
		if name != "client_version" {
			return ""
		}
	}
	if r.URL.Path == "/backend-api/codex/models" || query.Get("client_version") != "" {
		return CatalogCodex
	}
	return CatalogPlain
}

func (m *Manager) enterDiscovery(c *gin.Context) bool {
	if !discoveryCollection(c) {
		return false
	}
	view := m.discoverySnapshot()
	index, ok := m.store.(interface{ HasDiscoveryKey(string) bool })
	if view == nil || !ok || !index.HasDiscoveryKey(requestKey(c.Request)) {
		return false
	}
	original := c.Request.Clone(c.Request.Context())
	c.Set(discoveryContextKey, discoveryRequest{
		lookup: func(id int64, kind string) []byte {
			// A queue or an install may cross this request. Never combine
			// group bodies from two publications, or reuse an expired lease.
			if !m.Ready() || m.discoverySnapshot() != view {
				return nil
			}
			return view.Get(id, kind)
		},
		forward: func(c *gin.Context) {
			if !m.Ready() {
				unavailable(c)
				return
			}
			m.proxy.ServeHTTP(c.Writer, original)
			c.Abort()
		},
	})
	c.Next()
	return true
}
