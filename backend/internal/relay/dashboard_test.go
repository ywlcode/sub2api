package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardShowsOnlyThreeRollingAverages(t *testing.T) {
	h := &Handler{}
	for _, path := range []string{"/relay", "/relay/"} {
		response := httptest.NewRecorder()
		h.StatsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), "暂无样本") {
			t.Fatal("empty dashboard reports zero")
		}
	}
	h.Timings.record(100*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond)
	response := httptest.NewRecorder()
	h.StatsHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/relay", nil))
	html := response.Body.String()
	for _, want := range []string{"100.00 ms", "200.00 ms", "300.00 ms", "1 / 2000", "平均上传耗时", "平均本地处理耗时", "平均火山首 token 耗时"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(html, "request_id") || strings.Contains(html, "P95") {
		t.Fatal("dashboard includes request details or extra metrics")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("dashboard can become stale")
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/", 404}, {"POST", "/relay", 405}, {"GET", "/relay/stats", 404}} {
		r := httptest.NewRecorder()
		h.StatsHandler().ServeHTTP(r, httptest.NewRequest(tc.method, tc.path, nil))
		if r.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, r.Code)
		}
	}
}
