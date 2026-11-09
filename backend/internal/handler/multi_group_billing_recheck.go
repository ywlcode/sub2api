package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// billingErrorDetailsAfterWait is only for the billing recheck after concurrency
// admission. Another request may exhaust the selected subscription while this
// request waits. For a multi-group key, let the client retry with fresh group
// selection, without probing other groups or changing this request's billing
// identity. Initial authorization and other billing errors keep their status.
func billingErrorDetailsAfterWait(err error, apiKey *service.APIKey) (status int, code, message string, retryAfter int) {
	status, code, message, retryAfter = billingErrorDetails(err)
	if len(apiKey.ConfiguredGroupIDs()) > 1 &&
		(errors.Is(err, service.ErrDailyLimitExceeded) ||
			errors.Is(err, service.ErrWeeklyLimitExceeded) ||
			errors.Is(err, service.ErrMonthlyLimitExceeded)) {
		status = http.StatusBadGateway
	}
	return
}
