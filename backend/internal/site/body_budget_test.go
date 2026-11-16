package site

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSiteBodyBudgetCoversIngressAndReleasesAfterRequest(t *testing.T) {
	m, _, _, _ := pairedSites(t)
	m.current.Store(&lease{expires: time.Now().Add(ReplicaTTL)})
	body := []byte(`{"model":"deepseek-test","input":"` + strings.Repeat("x", 1000) + `"}`)
	m.BodyBudget = pkghttputil.NewBodyMemoryBudget(int64(len(body) + 100))
	entered, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	r := gin.New()
	r.Use(m.Ingress())
	r.POST("/v1/responses", func(c *gin.Context) {
		decoded, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		require.NoError(t, err)
		require.Equal(t, body, decoded, "local routing must reuse the budgeted bytes")
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-time.After(3 * time.Second):
			t.Error("request was never released")
		}
		c.Status(http.StatusOK)
	})
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request failed to enter")
	}
	busy := call()
	require.Equal(t, http.StatusServiceUnavailable, busy.Code)
	require.Equal(t, "1", busy.Header().Get("Retry-After"))
	close(release)
	require.Equal(t, http.StatusOK, (<-done).Code)
	require.Equal(t, http.StatusOK, call().Code, "completed request must release its entire reservation")
}
