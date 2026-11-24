package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMyModerationRecordsHandler(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		subject     any
		status      int
		page, size  int
	}{
		{"defaults", "", middleware.AuthSubject{UserID: 17}, 200, 1, 10},
		{"owner from subject", "?page=2&page_size=1&user_id=17", middleware.AuthSubject{UserID: 23}, 200, 2, 1},
		{"upper limits", "?page=100000&page_size=100", middleware.AuthSubject{UserID: 17}, 200, 100000, 100},
		{"missing subject", "?user_id=17", nil, 401, 0, 0},
		{"invalid subject type", "?user_id=17", "17", 401, 0, 0},
		{"zero subject id", "?user_id=17", middleware.AuthSubject{}, 401, 0, 0},
		{"negative subject id", "", middleware.AuthSubject{UserID: -1}, 401, 0, 0},
		{"zero page", "?page=0", middleware.AuthSubject{UserID: 17}, 400, 0, 0},
		{"page too large", "?page=100001", middleware.AuthSubject{UserID: 17}, 400, 0, 0},
		{"invalid page", "?page=abc", middleware.AuthSubject{UserID: 17}, 400, 0, 0},
		{"zero size", "?page_size=0", middleware.AuthSubject{UserID: 17}, 400, 0, 0},
		{"size too large", "?page_size=101", middleware.AuthSubject{UserID: 17}, 400, 0, 0},
		{"fractional size", "?page_size=1.5", middleware.AuthSubject{UserID: 17}, 400, 0, 0},
		{"service error", "", middleware.AuthSubject{UserID: 17}, 500, 1, 10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/user/security/moderation"+tt.query, nil)
			if tt.subject != nil {
				c.Set(string(middleware.ContextKeyUser), tt.subject)
			}
			called := false
			result := &service.MyModerationRecordsPage{
				Page: tt.page, PageSize: tt.size, Total: 1,
				Items: []service.MyModerationRecord{{ID: 42}},
			}
			serveMyModerationRecords(c, func(ctx context.Context, userID int64, page, size int) (*service.MyModerationRecordsPage, error) {
				called = true
				require.Equal(t, c.Request.Context(), ctx)
				require.Equal(t, tt.subject, middleware.AuthSubject{UserID: userID})
				require.Equal(t, tt.page, page)
				require.Equal(t, tt.size, size)
				if tt.status == http.StatusInternalServerError {
					return nil, errors.New("moderation records unavailable")
				}
				return result, nil
			})
			require.Equal(t, tt.status, w.Code, w.Body.String())
			require.Equal(t, tt.page > 0, called)
			if tt.status == http.StatusOK {
				var envelope struct {
					Data json.RawMessage `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
				want, err := json.Marshal(result)
				require.NoError(t, err)
				require.JSONEq(t, string(want), string(envelope.Data))
			}
		})
	}
}
