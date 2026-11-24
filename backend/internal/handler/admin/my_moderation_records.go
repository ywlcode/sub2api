package admin

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ContentModerationHandler) MyModerationRecords(c *gin.Context) {
	serveMyModerationRecords(c, h.service.MyModerationRecords)
}

func serveMyModerationRecords(c *gin.Context, list func(context.Context, int64, int, int) (*service.MyModerationRecordsPage, error)) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "请先登录")
		return
	}
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 || page > 100000 {
		response.BadRequest(c, "page 必须为 1 到 100000 之间的整数")
		return
	}
	size, err := strconv.Atoi(c.DefaultQuery("page_size", "10"))
	if err != nil || size < 1 || size > 100 {
		response.BadRequest(c, "page_size 必须为 1 到 100 之间的整数")
		return
	}
	records, err := list(c.Request.Context(), subject.UserID, page, size)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, records)
}
