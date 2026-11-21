package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSecondaryProcessingProxyAllProviders(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.URL.Host) }))
	defer proxy.Close()
	routes, err := site.NewEgressRoutes(site.EgressSettings{Processing: proxy.URL, Forward: "http://127.0.0.1:1"})
	require.NoError(t, err)
	cfg := &config.Config{Site: site.Config{Mode: site.Secondary, Egress: routes}}
	client := NewHTTPUpstream(cfg)
	for _, dest := range []string{"volc.invalid", "other.invalid"} {
		req, _ := http.NewRequest("GET", "http://"+dest+"/v1", nil)
		res, err := client.Do(req, "http://127.0.0.1:2", 19, 2)
		require.NoError(t, err)
		data, err := io.ReadAll(res.Body)
		require.NoError(t, res.Body.Close())
		require.NoError(t, err)
		require.Equal(t, dest, string(data))
	}
}
