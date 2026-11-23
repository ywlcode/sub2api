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
		Month            string `json:"month"`
		RequestID        string `json:"request_id"`
		ExpectedRevision string `json:"expected_report_revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF {
		response.BadRequest(c, "Invalid monthly usage report request")
		return
	}
	job, err := h.opsService.StartManualMonthlyUsage(c.Request.Context(), req.Month, req.RequestID, req.ExpectedRevision)
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
		response.ErrorWithDetails(c, http.StatusNotFound, "Monthly usage report job not found", "MONTHLY_USAGE_REPORT_JOB_NOT_FOUND", nil)
	case errors.Is(err, service.ErrMonthlyUsageBusy):
		response.ErrorWithDetails(c, http.StatusConflict, "A monthly usage report is already running", "MONTHLY_USAGE_REPORT_BUSY", nil)
	case errors.Is(err, service.ErrMonthlyUsageConflict):
		response.ErrorWithDetails(c, http.StatusConflict, "Request ID already belongs to a different report month", "MONTHLY_USAGE_REPORT_ID_CONFLICT", nil)
	case errors.Is(err, service.ErrReportRevisionRequired):
		response.ErrorWithDetails(c, http.StatusPreconditionRequired, service.ErrReportRevisionRequired.Error(), "MONTHLY_USAGE_REPORT_REVISION_REQUIRED", nil)
	case errors.Is(err, service.ErrReportRevisionConflict):
		response.ErrorWithDetails(c, http.StatusConflict, service.ErrReportRevisionConflict.Error(), "MONTHLY_USAGE_REPORT_REVISION_CONFLICT", nil)
	case errors.Is(err, service.ErrMonthlyUsageInvalid), errors.Is(err, service.ErrEmailNotConfigured):
		response.BadRequest(c, "Invalid monthly usage report request or email configuration")
	default:
		response.InternalError(c, "Monthly usage report operation failed")
	}
}
