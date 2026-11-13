package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type SiteReconciliationHandler struct {
	store service.SiteReconciliationStore
}

func NewSiteReconciliationHandler(store service.SiteReconciliationStore) *SiteReconciliationHandler {
	return &SiteReconciliationHandler{store: store}
}

func (h *SiteReconciliationHandler) List(c *gin.Context) {
	state := c.Query("state")
	if state != "" && state != "pending" && state != "retrying" && state != "settled" && state != "waived" {
		response.BadRequest(c, "Invalid reconciliation state")
		return
	}
	after, err := strconv.ParseInt(c.DefaultQuery("after", "0"), 10, 64)
	if err != nil || after < 0 {
		response.BadRequest(c, "Invalid event cursor")
		return
	}
	_, limit := response.ParsePagination(c)
	rows, err := h.store.ListSiteRejections(c.Request.Context(), c.Param("node"), state, after, limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, rows)
}

func (h *SiteReconciliationHandler) Get(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("event"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid event ID")
		return
	}
	r, err := h.store.GetSiteRejection(c.Request.Context(), c.Param("node"), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, r)
}

func (h *SiteReconciliationHandler) Resolve(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("event"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid event ID")
		return
	}
	var input struct {
		Action string `json:"action" binding:"required,oneof=retry settled waived"`
		Note   string `json:"note" binding:"required,max=2000"`
	}
	if err = c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Specify retry, settled or waived and a reconciliation note")
		return
	}
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	r, err := h.store.ResolveSiteRejection(c.Request.Context(), c.Param("node"), id, input.Action, input.Note, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, r)
}
