package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBodyLimitFromUpdatesWithoutRestart(t *testing.T) {
	limit := int64(4)
	router := gin.New()
	router.Use(RequestBodyLimitFrom(func() int64 { return limit }))
	router.POST("/", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(413)
		} else {
			c.Status(200)
		}
	})
	for _, tc := range []struct {
		limit  int64
		status int
	}{{4, 413}, {8, 200}, {3, 413}} {
		limit = tc.limit
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader("hello")))
		require.Equal(t, tc.status, w.Code)
	}
}
