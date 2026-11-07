package middleware

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type jwtUserSnapshotKey struct{}

// Retain the already validated user only for this HTTP request. This is not a
// cross-request authentication or balance cache; AuthSubject stays minimal.
func setJWTUserSnapshot(c *gin.Context, user *service.User) {
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), jwtUserSnapshotKey{}, user))
}

// JWTUserSnapshot returns a read-only view for handlers that only display the
// current user's profile. Never reuse it after a mutation or for another user.
func JWTUserSnapshot(c *gin.Context) (*service.User, bool) {
	if c == nil || c.Request == nil {
		return nil, false
	}
	subject, ok := GetAuthSubjectFromContext(c)
	if !ok {
		return nil, false
	}
	user, ok := c.Request.Context().Value(jwtUserSnapshotKey{}).(*service.User)
	if !ok || user == nil || user.ID != subject.UserID {
		return nil, false
	}
	return user, true
}
