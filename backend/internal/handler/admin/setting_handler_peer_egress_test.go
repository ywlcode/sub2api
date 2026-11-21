package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeerEgressAdminIsolation(t *testing.T) {
	repo := &pr612AdminSettingRepo{settingHandlerRepoStub{values: map[string]string{}}}
	svc := service.NewSettingService(repo, &config.Config{Site: site.Config{Mode: site.Primary, Peers: []site.Peer{{ID: "office"}, {ID: "other"}}}})
	h := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
	r := gin.New()
	r.PUT("/egress/:node", h.UpdatePeerEgress)
	r.GET("/egress", h.ListPeerEgress)
	put := func(node, body string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/egress/"+node, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w.Code
	}
	body := `{"processing_proxy_url":"http://proxy:8101","forward_proxy_url":"http://proxy:8102"}`
	require.Equal(t, 200, put("office", body))
	require.Equal(t, 404, put("absent", body))
	require.Equal(t, 400, put("office", `{"processing_proxy_url":"","forward_proxy_url":"http://user:secret@proxy:8123"}`))
	require.Equal(t, 400, put("office", `{"processing_proxy_url":""}`))
	p, e := svc.GetPeerEgress(t.Context(), "other")
	require.NoError(t, e)
	require.Empty(t, p)
	p, e = svc.GetPeerEgress(t.Context(), "office")
	require.NoError(t, e)
	require.Equal(t, "http://proxy:8102", p.Forward)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/egress", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"node":"office"`)
}
