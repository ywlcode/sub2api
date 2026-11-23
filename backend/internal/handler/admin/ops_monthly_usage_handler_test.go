//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const monthlyJobID = "febbc0a1-51d3-4644-8389-ec2db961f33e"
const monthlyJobRequest = `{"month":"2024-02","request_id":"` + monthlyJobID + `"}`

type monthlyHandlerStore struct {
	service.OpsRepository
	service.OpsManualMonthlyUsageRepository
	job *service.OpsMonthlyUsageJob
	err error
}

func (s *monthlyHandlerStore) GetManualMonthlyUsage(_ context.Context, id string) (*service.OpsMonthlyUsageJob, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.job != nil && id == s.job.ID {
		return s.job, nil
	}
	return nil, service.ErrMonthlyUsageJobNotFound
}

func monthlyUsageHandlerRouter(store *monthlyHandlerStore, settings *testSettingRepo) *gin.Engine {
	svc := service.NewOpsService(store, settings, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpsHandler(svc)
	r := gin.New()
	r.POST("/send", h.StartManualMonthlyUsage)
	r.GET("/jobs/:id", h.ManualMonthlyUsageStatus)
	r.GET("/config", h.GetEmailNotificationConfig)
	r.PUT("/config", h.UpdateEmailNotificationConfig)
	r.PUT("/config/conditional", h.UpdateEmailNotificationConfig)
	return r
}

func monthlyUsageRequest(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestManualMonthlyUsageHandlerInvalidRequest(t *testing.T) {
	r := monthlyUsageHandlerRouter(&monthlyHandlerStore{err: errors.New("invalid input reached repository")}, newTestSettingRepo())
	for _, tc := range []struct{ name, body string }{
		{"malformed", "{"},
		{"trailing JSON", monthlyJobRequest + "{}"},
		{"missing month", `{"request_id":"` + monthlyJobID + `"}`},
		{"invalid calendar month", strings.Replace(monthlyJobRequest, "2024-02", "2024-13", 1)},
		{"incomplete calendar month", strings.Replace(monthlyJobRequest, "2024-02", "9999-12", 1)},
		{"missing request ID", `{"month":"2024-02"}`},
		{"invalid request ID", strings.Replace(monthlyJobRequest, monthlyJobID, "private@example.org", 1)},
		{"recipient override", `{"month":"2024-02","request_id":"` + monthlyJobID + `","recipients":["private@example.org"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := monthlyUsageRequest(r, http.MethodPost, "/send", tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "private@example.org")
		})
	}
	require.Equal(t, http.StatusBadRequest, monthlyUsageRequest(r, http.MethodGet, "/jobs/private@example.org", "").Code)
}

func TestManualMonthlyUsageHandlerConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"no saved configuration", ""},
		{"disabled", `{"report":{"enabled":false,"recipients":["private@example.org"]}}`},
		{"no saved recipients", `{"report":{"enabled":true,"recipients":[]}}`},
		{"invalid saved recipient", `{"report":{"enabled":true,"recipients":["Private <private@example.org>"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := newTestSettingRepo()
			if tc.config != "" {
				settings.values[service.SettingKeyOpsEmailNotificationConfig] = tc.config
			}
			r := monthlyUsageHandlerRouter(&monthlyHandlerStore{}, settings)
			w := monthlyUsageRequest(r, http.MethodPost, "/send", monthlyRequestWithRevision(loadEmailRevision(t, r)))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "private@example.org")
			require.Equal(t, http.StatusNotFound, monthlyUsageRequest(r, http.MethodGet, "/jobs/"+monthlyJobID, "").Code)
		})
	}
}

func TestManualMonthlyUsageHandlerDisabled(t *testing.T) {
	settings := newTestSettingRepo()
	settings.values[service.SettingKeyOpsMonitoringEnabled] = "false"
	r := monthlyUsageHandlerRouter(&monthlyHandlerStore{}, settings)
	want := monthlyUsageRequest(r, http.MethodGet, "/config", "")
	require.Equal(t, http.StatusNotFound, want.Code)
	for _, w := range []*httptest.ResponseRecorder{
		monthlyUsageRequest(r, http.MethodPost, "/send", "{}"),
		monthlyUsageRequest(r, http.MethodGet, "/jobs/invalid", ""),
	} {
		require.Equal(t, want.Code, w.Code)
		require.JSONEq(t, want.Body.String(), w.Body.String())
	}
}

func TestManualMonthlyUsageHandlerExistingJob(t *testing.T) {
	job := &service.OpsMonthlyUsageJob{ID: monthlyJobID, Month: "2024-02", Status: "running", Total: 2}
	r := monthlyUsageHandlerRouter(&monthlyHandlerStore{job: job}, newTestSettingRepo())
	for _, tc := range []struct {
		status   string
		postCode int
	}{
		{"running", http.StatusAccepted}, {"sent", http.StatusOK}, {"failed", http.StatusOK}, {"uncertain", http.StatusOK},
	} {
		t.Run(tc.status, func(t *testing.T) {
			job.Status = tc.status
			post := monthlyUsageRequest(r, http.MethodPost, "/send", monthlyJobRequest)
			require.Equal(t, tc.postCode, post.Code)
			get := monthlyUsageRequest(r, http.MethodGet, "/jobs/"+monthlyJobID, "")
			require.Equal(t, http.StatusOK, get.Code)
			for _, w := range []*httptest.ResponseRecorder{post, get} {
				var envelope responseEnvelope
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
				require.Zero(t, envelope.Code)
				require.JSONEq(t, fmt.Sprintf(`{"id":%q,"month":"2024-02","status":%q,"total":2,"accepted":0,"failed":0,"uncertain":0}`, monthlyJobID, tc.status), string(envelope.Data))
			}
		})
	}
	conflict := monthlyUsageRequest(r, http.MethodPost, "/send", strings.Replace(monthlyJobRequest, "2024-02", "2024-03", 1))
	require.Equal(t, http.StatusConflict, conflict.Code)
	require.Contains(t, conflict.Body.String(), `"reason":"MONTHLY_USAGE_REPORT_ID_CONFLICT"`)
}

func TestManualMonthlyUsageHandlerRedactsServiceErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{service.ErrMonthlyUsageBusy, http.StatusConflict},
		{service.ErrMonthlyUsageConflict, http.StatusConflict},
		{service.ErrMonthlyUsageJobNotFound, http.StatusNotFound},
		{service.ErrMonthlyUsageInvalid, http.StatusBadRequest},
		{service.ErrEmailNotConfigured, http.StatusBadRequest},
		{errors.New("internal DB detail"), http.StatusInternalServerError},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			store := &monthlyHandlerStore{err: fmt.Errorf("private@example.org: %w", tc.err)}
			r := monthlyUsageHandlerRouter(store, newTestSettingRepo())
			var w *httptest.ResponseRecorder
			if tc.err == service.ErrMonthlyUsageJobNotFound {
				w = monthlyUsageRequest(r, http.MethodGet, "/jobs/"+monthlyJobID, "")
			} else {
				w = monthlyUsageRequest(r, http.MethodPost, "/send", monthlyJobRequest)
			}
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.err == service.ErrMonthlyUsageBusy {
				require.Contains(t, w.Body.String(), `"reason":"MONTHLY_USAGE_REPORT_BUSY"`)
			}
			require.NotContains(t, w.Body.String(), "private@example.org")
			require.NotContains(t, w.Body.String(), "internal DB detail")
		})
	}
}

func loadEmailRevision(t *testing.T, r *gin.Engine) string {
	t.Helper()
	w := monthlyUsageRequest(r, http.MethodGet, "/config", "")
	require.Equal(t, http.StatusOK, w.Code)
	var result struct {
		Data service.OpsEmailNotificationConfig `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.NotEmpty(t, result.Data.ReportRevision)
	return result.Data.ReportRevision
}

func monthlyRequestWithRevision(revision string) string {
	return fmt.Sprintf(`{"month":"2024-02","request_id":%q,"expected_report_revision":%q}`, monthlyJobID, revision)
}

func (s *testSettingRepo) CompareAndSetOpsEmailConfig(_ context.Context, previous *string, next string) error {
	raw, found := s.values[service.SettingKeyOpsEmailNotificationConfig]
	if (previous == nil && found) || (previous != nil && (!found || raw != *previous)) {
		return service.ErrReportRevisionConflict
	}
	s.values[service.SettingKeyOpsEmailNotificationConfig] = next
	return nil
}

func TestManualMonthlyUsageRevisionContract(t *testing.T) {
	settings := newTestSettingRepo()
	store := &monthlyHandlerStore{}
	r := monthlyUsageHandlerRouter(store, settings)
	revision := loadEmailRevision(t, r)
	save := func(route, rev string, enabled bool, to string) *httptest.ResponseRecorder {
		return monthlyUsageRequest(r, http.MethodPut, route, fmt.Sprintf(
			`{"expected_report_revision":%q,"report":{"enabled":true,"monthly_usage_enabled":%t,"recipients":[%q]}}`,
			rev, enabled, to))
	}
	w := save("/config/conditional", revision, true, "current@example.org")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	current := loadEmailRevision(t, r)
	require.NotEqual(t, revision, current)
	require.Contains(t, w.Body.String(), current)
	for _, route := range []string{"/config", "/config/conditional"} {
		require.Equal(t, http.StatusConflict, save(route, revision, false, "stale@example.org").Code)
		require.Equal(t, http.StatusPreconditionRequired, save(route, "", false, "stale@example.org").Code)
	}
	require.Equal(t, current, loadEmailRevision(t, r))
	for _, tc := range []struct {
		body   string
		status int
		reason string
	}{
		{monthlyJobRequest, 428, "MONTHLY_USAGE_REPORT_REVISION_REQUIRED"},
		{monthlyRequestWithRevision(revision), 409, "MONTHLY_USAGE_REPORT_REVISION_CONFLICT"},
	} {
		w = monthlyUsageRequest(r, http.MethodPost, "/send", tc.body)
		require.Equal(t, tc.status, w.Code)
		var result struct {
			Reason string `json:"reason"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.Equal(t, tc.reason, result.Reason)
	}
	store.job = &service.OpsMonthlyUsageJob{ID: monthlyJobID, Month: "2024-02", Status: "sent", Total: 1, Accepted: 1}
	require.Equal(t, http.StatusOK, monthlyUsageRequest(r, http.MethodPost, "/send", monthlyRequestWithRevision(revision)).Code)
}
