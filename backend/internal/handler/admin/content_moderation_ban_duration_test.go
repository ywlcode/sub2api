package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContentModerationBanDurationHoursHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &moderationEngineSettings{value: `{"ban_threshold":7}`}
	handler := NewContentModerationHandler(service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil))
	router := gin.New()
	router.GET("/config", handler.GetConfig)
	router.PUT("/config", handler.UpdateConfig)
	request := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	assertHours := func(w *httptest.ResponseRecorder, hours int) {
		t.Helper()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var result struct {
			Data struct {
				Hours     int `json:"ban_duration_hours"`
				Threshold int `json:"ban_threshold"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.Equal(t, hours, result.Data.Hours)
		require.Equal(t, 7, result.Data.Threshold)
	}

	assertHours(request(http.MethodGet, ""), 24)
	assertHours(request(http.MethodPut, `{"ban_duration_hours":1}`), 1)
	assertHours(request(http.MethodPut, `{"ban_duration_hours":8760}`), 8760)
	assertHours(request(http.MethodPut, `{"auto_ban_enabled":false}`), 8760)
	assertHours(request(http.MethodPut, `{"ban_duration_hours":null}`), 8760)
	assertHours(request(http.MethodGet, ""), 8760)

	saved := settings.value
	for _, value := range []string{"0", "-1", "8761", "1.5", `"24"`} {
		w := request(http.MethodPut, `{"ban_duration_hours":`+value+`}`)
		require.Equal(t, http.StatusBadRequest, w.Code, "duration %s: %s", value, w.Body.String())
		require.Equal(t, saved, settings.value)
	}
}
