package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpsHandler) StartManualMonthlyUsage(c *gin.Context) {
	if !h.requireOpsMonitoring(c) {
		return
	}
	var req struct {
		Month     string `json:"month"`
		RequestID string `json:"request_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF {
		response.BadRequest(c, "Invalid monthly usage report request")
		return
	}
	job, err := h.opsService.StartManualMonthlyUsage(c.Request.Context(), req.Month, req.RequestID)
	if err != nil {
		monthlyUsageError(c, err)
		return
	}
	switch job.Status {
	case "sent", "failed", "uncertain":
		response.Success(c, job)
	default:
		response.Accepted(c, job)
	}
}

func (h *OpsHandler) ManualMonthlyUsageStatus(c *gin.Context) {
	if !h.requireOpsMonitoring(c) {
		return
	}
	job, err := h.opsService.ManualMonthlyUsageStatus(c.Request.Context(), c.Param("id"))
	if err != nil {
		monthlyUsageError(c, err)
		return
	}
	response.Success(c, job)
}

// Service errors may wrap SMTP or database details. Only return fixed public messages.
func monthlyUsageError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrMonthlyUsageJobNotFound):
		response.NotFound(c, "Monthly usage report job not found")
	case errors.Is(err, service.ErrMonthlyUsageBusy):
		response.Error(c, http.StatusConflict, "A monthly usage report is already running")
	case errors.Is(err, service.ErrMonthlyUsageConflict):
		response.Error(c, http.StatusConflict, "Request ID already belongs to a different report month")
	case errors.Is(err, service.ErrMonthlyUsageInvalid), errors.Is(err, service.ErrEmailNotConfigured):
		response.BadRequest(c, "Invalid monthly usage report request or email configuration")
	default:
		response.InternalError(c, "Monthly usage report operation failed")
	}
}
