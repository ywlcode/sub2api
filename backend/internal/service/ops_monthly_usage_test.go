package service

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/monthlyusage"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestMonthlyUsageCalendarAndCatchUp(t *testing.T) {
	for _, tc := range []struct{ instant, first, last, want string }{
		{"2026-11-30T22:14:59Z", "2026-11", "", ""},
		{"2026-11-30T22:15:00Z", "2026-11", "", "2026-11"},
		{"2027-01-01T06:15:00+08:00", "2026-11", "2026-11", "2026-12"},
		{"2027-02-06T20:00:00Z", "2026-11", "2026-11", "2026-12"},
		{"2027-02-06T20:00:00Z", "2027-02", "2026-11", ""},
		{"2024-03-02T05:00:00Z", "2024-02", "", "2024-02"},
	} {
		now, err := time.Parse(time.RFC3339, tc.instant)
		require.NoError(t, err)
		got, err := dueMonthlyUsagePeriod(now, tc.first, tc.last)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	require.Equal(t, "2026-11", firstMonthlyUsagePeriod(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)))
	require.Equal(t, "2026-12", firstMonthlyUsagePeriod(time.Date(2026, 12, 31, 22, 15, 0, 0, time.UTC)))
}

func TestMonthlyUsageSettingsCompatibility(t *testing.T) {
	repo := newNotificationEmailMemorySettingRepo()
	o := OpsService{settingRepo: repo, opsRepo: &monthlyTestStore{}}
	ctx := context.Background()
	update := func(body string) (*OpsEmailNotificationConfig, error) {
		var req OpsEmailNotificationConfigUpdateRequest
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		return o.UpdateEmailNotificationConfig(ctx, &req)
	}
	cfg, err := o.GetEmailNotificationConfig(ctx)
	require.NoError(t, err)
	require.False(t, cfg.Report.MonthlyUsageEnabled)
	cfg, err = update(`{"report":{"enabled":true,"recipients":["a@example.org"],"weekly_summary_enabled":true,"monthly_usage_enabled":true,"monthly_usage_first_period":"2000-01","monthly_usage_department_attribute_id":23}}`)
	require.NoError(t, err)
	start := cfg.Report.MonthlyUsageFirstPeriod
	require.Equal(t, firstMonthlyUsagePeriod(time.Now()), start)
	cfg, err = update(`{"report":{"enabled":true,"weekly_summary_enabled":true}}`)
	require.NoError(t, err)
	require.True(t, cfg.Report.MonthlyUsageEnabled)
	require.Equal(t, start, cfg.Report.MonthlyUsageFirstPeriod)
	require.EqualValues(t, 23, cfg.Report.MonthlyUsageDepartmentAttributeID, "older clients do not reset the saved field")
	_, err = update(`{"report":{"monthly_usage_department_attribute_id":12}}`)
	require.ErrorContains(t, err, "user attribute")
	cfg, err = o.GetEmailNotificationConfig(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 23, cfg.Report.MonthlyUsageDepartmentAttributeID)
	// The saved definition has since disappeared. A full-config client can
	// still disable the reports without first supplying a different field.
	o.opsRepo = nil
	cfg, err = update(`{"report":{"enabled":false,"monthly_usage_department_attribute_id":23}}`)
	require.NoError(t, err)
	require.False(t, cfg.Report.Enabled)
	require.EqualValues(t, 23, cfg.Report.MonthlyUsageDepartmentAttributeID)
	_, err = update(`{"report":{"enabled":true,"recipients":[],"monthly_usage_enabled":true}}`)
	require.Error(t, err)
	cfg, err = update(`{"report":{"enabled":true,"monthly_usage_enabled":false,"weekly_summary_enabled":true,"monthly_usage_department_attribute_id":0}}`)
	require.NoError(t, err)
	require.False(t, cfg.Report.MonthlyUsageEnabled)
	require.Zero(t, cfg.Report.MonthlyUsageDepartmentAttributeID)
	require.True(t, cfg.Report.WeeklySummaryEnabled)
}

type monthlyTestStore struct {
	OpsRepository
	states       map[string]string
	settings     *notificationEmailMemorySettingRepo
	finishErr    error
	built        []string
	attributeIDs []int64
	onBuild      func()
}

func (r *monthlyTestStore) MonthlyUsageDepartmentAttributeAvailable(_ context.Context, id int64) (bool, error) {
	return id == 23, nil
}
func (r *monthlyTestStore) BuildMonthlyUsageReportWithCoverage(_ context.Context, month, site string, departmentAttributeID int64, w io.Writer) (string, error) {
	r.built = append(r.built, month)
	r.attributeIDs = append(r.attributeIDs, departmentAttributeID)
	if r.onBuild != nil {
		r.onBuild()
	}
	_, err := io.WriteString(w, "workbook for "+month)
	return "", err
}
func (r *monthlyTestStore) MonthlyUsageDelivery(_ context.Context, month, to string) (MonthlyUsageDeliveryState, error) {
	return MonthlyUsageDeliveryState(r.states[month+to]), nil
}
func (r *monthlyTestStore) NextMonthlyUsageRetry(_ context.Context, first, last string, recipients []string) (string, error) {
	for key, state := range r.states {
		if state == "rejected" && key[:7] >= first && key[:7] <= last && slices.Contains(recipients, key[7:]) {
			return key[:7], nil
		}
	}
	return "", nil
}
func (r *monthlyTestStore) AdvanceMonthlyUsageDispatch(ctx context.Context, month, raw string) (bool, error) {
	current, err := r.settings.GetValue(ctx, SettingKeyOpsEmailNotificationConfig)
	if err != nil || current != raw {
		return false, err
	}
	return true, r.settings.Set(ctx, monthlyUsageCheckpoint, month)
}
func (r *monthlyTestStore) ClaimMonthlyUsageEmailForConfig(ctx context.Context, month, to, raw string) (bool, error) {
	current, err := r.settings.GetValue(ctx, SettingKeyOpsEmailNotificationConfig)
	if err != nil || current != raw {
		return false, err
	}
	if state := r.states[month+to]; state != "" && state != "rejected" {
		return false, nil
	}
	r.states[month+to] = "sending"
	return true, nil
}
func (r *monthlyTestStore) FinishMonthlyUsageEmail(_ context.Context, month, to string, state MonthlyUsageDeliveryState) error {
	if r.finishErr != nil {
		return r.finishErr
	}
	r.states[month+to] = string(state)
	return nil
}

func TestMonthlyUsageDeliveryRestartAndRecipients(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 6, 15, 0, 0, monthlyUsageZone)
	settings := newNotificationEmailMemorySettingRepo()
	report := `{"alert":{"recipients":["alert@example.org"]},"report":{"enabled":true,"recipients":["A@example.org","b@example.org","a@example.org"],"monthly_usage_enabled":true,"monthly_usage_first_period":"2026-11","monthly_usage_department_attribute_id":23}}`
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, report))
	store := &monthlyTestStore{settings: settings, states: map[string]string{}}
	s := &OpsScheduledReportService{opsService: &OpsService{opsRepo: store, settingRepo: settings}}
	var sent []string
	refuse := true
	send := func(_ context.Context, addr, subject, body, filename string, data []byte) (bool, error) {
		require.Contains(t, subject, "独立站")
		require.Contains(t, filename, "2026-11.xlsx")
		require.Equal(t, "workbook for 2026-11", string(data))
		sent = append(sent, addr)
		if addr == "b@example.org" && refuse {
			return false, errors.New("rejected")
		}
		return false, nil
	}
	_, err := s.monthlyUsageTick(ctx, now, send)
	require.ErrorContains(t, err, "rejected")
	refuse = false
	// A fresh application instance sees the first recipient's durable receipt.
	s = &OpsScheduledReportService{opsService: s.opsService}
	period, err := s.monthlyUsageTick(ctx, now.Add(time.Hour), send)
	require.NoError(t, err)
	require.Equal(t, "2026-11", period)
	period, err = s.monthlyUsageTick(ctx, now.Add(2*time.Hour), send)
	require.NoError(t, err)
	require.Empty(t, period)
	require.Equal(t, []string{"a@example.org", "b@example.org", "b@example.org"}, sent)
	require.Equal(t, []string{"2026-11", "2026-11"}, store.built)
	require.Equal(t, []int64{23, 23}, store.attributeIDs)
}

func TestMonthlyUsageNoDuplicateOrFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 6, 15, 0, 0, monthlyUsageZone)
	settings := newNotificationEmailMemorySettingRepo()
	configure := func(recipients []string) {
		raw, err := json.Marshal(OpsEmailNotificationConfig{Report: OpsEmailReportConfig{Enabled: true, MonthlyUsageEnabled: true, MonthlyUsageFirstPeriod: "2026-11", Recipients: recipients}})
		require.NoError(t, err)
		require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, string(raw)))
	}
	store := &monthlyTestStore{settings: settings, states: map[string]string{}}
	s := &OpsScheduledReportService{opsService: &OpsService{settingRepo: settings, opsRepo: store}}
	never := func(context.Context, string, string, string, string, []byte) (bool, error) {
		t.Fatal("unexpected email")
		return false, nil
	}
	configure(nil)
	_, err := s.monthlyUsageTick(ctx, now, never)
	require.NoError(t, err)
	configure([]string{"a@example.org"})
	s.cfg = &config.Config{Site: site.Config{Mode: site.Secondary}}
	_, err = s.monthlyUsageTick(ctx, now, never)
	require.NoError(t, err)
	require.Empty(t, store.built)
	s.cfg = nil
	// Another instance is still sending: don't advance the checkpoint.
	store.states["2026-11a@example.org"] = "sending"
	period, err := s.monthlyUsageTick(ctx, now, never)
	require.NoError(t, err)
	require.Empty(t, period)
	require.Empty(t, store.built)
	delete(store.states, "2026-11a@example.org")
	_, err = s.monthlyUsageTick(ctx, now, func(context.Context, string, string, string, string, []byte) (bool, error) {
		return true, errors.New("receipt lost")
	})
	require.ErrorContains(t, err, "uncertain")
	require.Equal(t, "uncertain", store.states["2026-11a@example.org"])
	period, err = s.monthlyUsageTick(ctx, now.Add(time.Hour), never)
	require.NoError(t, err)
	require.Empty(t, period)
	require.Len(t, store.built, 1)
}

func TestMonthlyUsageRechecksRecipientsAfterExport(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	raw := func(to string) string {
		return `{"report":{"enabled":true,"recipients":["` + to + `"],"monthly_usage_enabled":true,"monthly_usage_first_period":"2026-11"}}`
	}
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, raw("old@example.org")))
	store := &monthlyTestStore{settings: settings, states: map[string]string{}, onBuild: func() {
		require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, raw("new@example.org")))
	}}
	s := &OpsScheduledReportService{opsService: &OpsService{opsRepo: store, settingRepo: settings}}
	period, err := s.monthlyUsageTick(ctx, time.Date(2026, 12, 1, 6, 15, 0, 0, monthlyUsageZone), func(_ context.Context, to, _, _, _ string, _ []byte) (bool, error) {
		require.Equal(t, "new@example.org", to)
		return false, nil
	})
	require.NoError(t, err)
	require.Equal(t, "2026-11", period)
	require.Equal(t, map[string]string{"2026-11new@example.org": "accepted"}, store.states)
}

func TestMonthlyUsageChangedDepartmentRetriesBeforeSending(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	const initial = `{"report":{"enabled":true,"recipients":["a@example.org"],"monthly_usage_enabled":true,"monthly_usage_first_period":"2026-11","monthly_usage_department_attribute_id":23}}`
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, initial))
	store := &monthlyTestStore{settings: settings, states: map[string]string{}, onBuild: func() {
		require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, strings.Replace(initial, ":23}", ":0}", 1)))
	}}
	s := &OpsScheduledReportService{opsService: &OpsService{settingRepo: settings, opsRepo: store}}
	now := time.Date(2026, 12, 1, 6, 15, 0, 0, monthlyUsageZone)
	sent := 0
	send := func(context.Context, string, string, string, string, []byte) (bool, error) {
		sent++
		return false, nil
	}
	period, err := s.monthlyUsageTick(ctx, now, send)
	require.NoError(t, err)
	require.Empty(t, period)
	require.Zero(t, sent)
	store.onBuild = nil
	period, err = s.monthlyUsageTick(ctx, now.Add(time.Hour), send)
	require.NoError(t, err)
	require.Equal(t, "2026-11", period)
	require.Equal(t, 1, sent)
	require.Equal(t, []int64{23, 0}, store.attributeIDs)
}

type monthlyWorkbookMailTestStore struct{ *monthlyTestStore }

func (s monthlyWorkbookMailTestStore) BuildMonthlyUsageReportWithCoverage(ctx context.Context, month, label string, _ int64, w io.Writer) (string, error) {
	const note = "历史覆盖无法确认：仅含现存明细。"
	return note, monthlyusage.BuildContext(ctx, w, label, map[string]int64{"研发": 2}, note, func(row func(monthlyusage.Row) error) error {
		return row(monthlyusage.Row{Day: month + "-28", UserID: "07", Username: "测试用户", Department: "研发", Model: "test-model", Requests: "4", InputTokens: "8000000000", OutputTokens: "2000000", CacheReadTokens: "1000", TotalTokens: "8002001000", ActualCost: "12.51",
			PlatformUsage: `{"gemini":["1","8000000000","0","0","8000000000","12"],"future":["3","0","2000000","1000","2001000","0.51"]}`})
	})
}

func TestMonthlyUsagePrimaryDeliversReadableWorkbook(t *testing.T) {
	ctx := context.Background()
	db := newNotificationEmailMemorySettingRepo()
	smtp := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, db.SetMultiple(ctx, smtp.settings()))
	require.NoError(t, db.Set(ctx, SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":true,"monthly_usage_enabled":true,"monthly_usage_first_period":"2024-02","recipients":["reader@example.org"]}}`))
	repo := monthlyWorkbookMailTestStore{&monthlyTestStore{settings: db, states: map[string]string{}}}
	s := &OpsScheduledReportService{cfg: &config.Config{Site: site.Config{Mode: site.Primary}}, opsService: &OpsService{opsRepo: repo, settingRepo: db}}
	period, err := s.monthlyUsageTick(ctx, time.Date(2024, 3, 1, 6, 15, 0, 0, monthlyUsageZone), NewEmailService(db, nil).SendMonthlyUsageExcel)
	require.NoError(t, err)
	require.Equal(t, "2024-02", period)
	require.Equal(t, int64(1), smtp.messageCount())
	msg, err := mail.ReadMessage(strings.NewReader(smtp.lastMessage()))
	require.NoError(t, err)
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	parts := multipart.NewReader(msg.Body, params["boundary"])
	message, err := parts.NextPart()
	require.NoError(t, err)
	body, err := io.ReadAll(message)
	require.NoError(t, err)
	require.Contains(t, string(body), "历史覆盖无法确认：仅含现存明细。")
	file, err := parts.NextPart()
	require.NoError(t, err)
	require.Equal(t, "Sub2API-主站-2024-02.xlsx", file.FileName())
	data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, file))
	require.NoError(t, err)
	book, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	for path, wants := range map[string][]string{
		"xl/workbook.xml":          {"部门Future平台月汇总", "部门Gemini平台月汇总"},
		"xl/worksheets/sheet1.xml": {"test-model"},
		"xl/worksheets/sheet2.xml": {"注册且未被封禁人数", "输入 Token (B)"},
		"xl/worksheets/sheet3.xml": {"注册且未被封禁人数", "输入 Token (B)"},
		"xl/worksheets/sheet4.xml": {"输入 Token (M)"},
	} {
		part, err := book.Open(path)
		require.NoError(t, err)
		b, err := io.ReadAll(part)
		require.NoError(t, err)
		require.NoError(t, part.Close())
		for _, want := range wants {
			require.Contains(t, string(b), want)
		}
	}
}

func TestMonthlyUsageRechecksListBetweenMessages(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	before := `{"report":{"enabled":true,"recipients":["a@example.org","revoked@example.org"],"monthly_usage_enabled":true,"monthly_usage_first_period":"2026-11"}}`
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, before))
	store := &monthlyTestStore{settings: settings, states: map[string]string{}}
	s := &OpsScheduledReportService{opsService: &OpsService{opsRepo: store, settingRepo: settings}}
	var sent []string
	send := func(_ context.Context, to, _, _, _ string, _ []byte) (bool, error) {
		sent = append(sent, to)
		if to == "a@example.org" {
			require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, strings.Replace(before, "revoked@", "new@", 1)))
		}
		return false, nil
	}
	period, err := s.monthlyUsageTick(ctx, time.Date(2026, 12, 1, 7, 0, 0, 0, monthlyUsageZone), send)
	require.NoError(t, err)
	require.Equal(t, "2026-11", period)
	require.Equal(t, []string{"a@example.org", "new@example.org"}, sent)
	require.Len(t, store.built, 1)
}

func TestMonthlyUsageRejectionDoesNotBlockLaterMonths(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	before := `{"report":{"enabled":true,"recipients":["bad@example.org","ok@example.org"],"monthly_usage_enabled":true,"monthly_usage_first_period":"2026-11"}}`
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, before))
	store := &monthlyTestStore{settings: settings, states: map[string]string{}}
	s := &OpsScheduledReportService{opsService: &OpsService{opsRepo: store, settingRepo: settings}}
	var sent []string
	refuse := true
	send := func(_ context.Context, to, _, _, _ string, data []byte) (bool, error) {
		sent = append(sent, string(data)+" "+to)
		if refuse && to == "bad@example.org" {
			return false, errors.New("550 refused")
		}
		return false, nil
	}
	now := time.Date(2026, 12, 1, 7, 0, 0, 0, monthlyUsageZone)
	for i, month := range []string{"2026-11", "2026-12"} {
		period, err := s.monthlyUsageTick(ctx, now.AddDate(0, i, 0), send)
		require.ErrorContains(t, err, "550 refused")
		require.Equal(t, month, period)
		require.Equal(t, "accepted", store.states[month+"ok@example.org"])
		require.Equal(t, "rejected", store.states[month+"bad@example.org"])
	}
	require.Len(t, sent, 4)
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, strings.Replace(before, "ok@", "new@", 1)))
	refuse = false
	for range 2 {
		_, err := s.monthlyUsageTick(ctx, now.AddDate(0, 1, 0).Add(time.Hour), send)
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []string{"workbook for 2026-11 bad@example.org", "workbook for 2026-12 bad@example.org"}, sent[4:])
}

func TestMonthlyUsageRefusalWithReceiptWriteFailure(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":true,"recipients":["bad@example.org"],"monthly_usage_enabled":true,"monthly_usage_first_period":"2026-11"}}`))
	store := &monthlyTestStore{settings: settings, states: map[string]string{}, finishErr: errors.New("database unreachable")}
	s := &OpsScheduledReportService{opsService: &OpsService{opsRepo: store, settingRepo: settings}}
	now := time.Date(2026, 12, 1, 7, 0, 0, 0, monthlyUsageZone)
	calls := 0
	send := func(context.Context, string, string, string, string, []byte) (bool, error) {
		calls++
		return false, errors.New("550 refused")
	}
	_, err := s.monthlyUsageTick(ctx, now, send)
	require.ErrorContains(t, err, "SMTP rejected; receipt update unconfirmed")
	require.ErrorContains(t, err, notificationEmailHash("bad@example.org"))
	require.ErrorContains(t, err, "550 refused")
	require.ErrorContains(t, err, "database unreachable")
	_, err = settings.GetValue(ctx, monthlyUsageCheckpoint)
	require.ErrorIs(t, err, ErrSettingNotFound)
	require.Equal(t, "sending", store.states["2026-11bad@example.org"])
	// The database classifies an old unresolved claim as uncertain on restart.
	store.states["2026-11bad@example.org"] = "uncertain"
	period, err := s.monthlyUsageTick(ctx, now.Add(time.Hour), send)
	require.Equal(t, "2026-11", period)
	require.ErrorContains(t, err, "1 uncertain")
	require.Equal(t, 1, calls)
}
