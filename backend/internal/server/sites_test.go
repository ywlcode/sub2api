package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStandaloneSiteSyncDoesNotFallThroughToSPA(t *testing.T) {
	r := gin.New()
	configureSites(r, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	r.NoRoute(func(c *gin.Context) { c.Data(200, "text/html", []byte("<html>panel</html>")) })
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, site.SyncPath, nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		require.Empty(t, w.Body.String())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "panel")
}
