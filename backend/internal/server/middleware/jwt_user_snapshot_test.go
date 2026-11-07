//go:build unit

package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestJWTUserSnapshotRequiresMatchingSubject(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	setJWTUserSnapshot(c, &service.User{ID: 7})
	_, ok := JWTUserSnapshot(c)
	require.False(t, ok)
	c.Set(string(ContextKeyUser), AuthSubject{UserID: 8})
	_, ok = JWTUserSnapshot(c)
	require.False(t, ok)
	c.Set(string(ContextKeyUser), AuthSubject{UserID: 7})
	user, ok := JWTUserSnapshot(c)
	require.True(t, ok)
	require.Equal(t, int64(7), user.ID)
}
