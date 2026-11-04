package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestPhasesCoverHTTPInferenceWithoutTimingWebSocketHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method, path string
		measured     bool
	}{
		{http.MethodPost, "/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", true},
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/v1/images/edits", true},
		{http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", true},
		{http.MethodGet, "/v1/responses", false},
		{http.MethodPost, "/api/v1/auth/login", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := gin.New()
			r.Use(RequestTiming())
			var snapshot *requesttiming.Snapshot
			r.Handle(tc.method, tc.path, func(c *gin.Context) {
				collector := requesttiming.From(c.Request.Context())
				if collector != nil {
					collector.WhenFinished(func(s requesttiming.Snapshot) { snapshot = &s })
				}
				_, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				c.String(200, "done")
			})
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, strings.NewReader("body")))
			if !tc.measured {
				require.Nil(t, snapshot)
				return
			}
			require.NotNil(t, snapshot)
			phases := requesttiming.DeriveRequestPhases(*snapshot)
			require.NotNil(t, phases.UserUploadMS)
			require.NotNil(t, phases.ProcessingMS)
		})
	}
}
