package service

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type manualMonthlyStoreStub struct {
	*monthlyTestStore
	mu       sync.Mutex
	jobs     map[string]OpsMonthlyUsageJob
	started  chan struct{}
	proceed  chan struct{}
	once     sync.Once
	onInsert func()
}

func (r *manualMonthlyStoreStub) BeginManualMonthlyUsage(ctx context.Context, id, month string, total int) (*OpsMonthlyUsageJob, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if job, ok := r.jobs[id]; ok {
		return &job, false, nil
	}
	for _, j := range r.jobs {
		if j.Status == "running" {
			return nil, false, ErrMonthlyUsageBusy
		}
	}
	j := OpsMonthlyUsageJob{ID: id, Month: month, Total: total, Status: "running"}
	r.jobs[id] = j
	if r.onInsert != nil {
		r.onInsert()
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
	}
	return &j, true, nil
}
func (r *manualMonthlyStoreStub) GetManualMonthlyUsage(_ context.Context, id string) (*OpsMonthlyUsageJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, ErrMonthlyUsageJobNotFound
	}
	return &j, nil
}
func (r *manualMonthlyStoreStub) UpdateManualMonthlyUsage(_ context.Context, j *OpsMonthlyUsageJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.ID] = *j
	return nil
}
func (r *manualMonthlyStoreStub) BuildMonthlyUsageReportWithCoverage(ctx context.Context, _, _ string, departmentAttributeID int64, w io.Writer) (string, error) {
	r.attributeIDs = append(r.attributeIDs, departmentAttributeID)
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.proceed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	_, err := io.WriteString(w, "test XLSX")
	return "本月非管理员已入账请求为 0。", err
}

func TestManualMonthlyUsageDeliversOncePerClickWithoutAutoEnabled(t *testing.T) {
	settings := newNotificationEmailMemorySettingRepo()
	ctx := context.Background()
	smtp := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, settings.SetMultiple(ctx, smtp.settings()))
	store := &manualMonthlyStoreStub{monthlyTestStore: &monthlyTestStore{states: map[string]string{}}, jobs: map[string]OpsMonthlyUsageJob{}, started: make(chan struct{}), proceed: make(chan struct{})}
	ops := &OpsService{settingRepo: settings, opsRepo: store}
	worker := NewOpsScheduledReportService(ops, nil, NewEmailService(settings, nil), nil, nil)
	worker.stopCtx, worker.stop = context.WithCancel(ctx)
	worker.manualOpen = true
	t.Cleanup(worker.Stop)
	id := uuid.NewString()
	_, err := ops.StartManualMonthlyUsage(ctx, "2024-02", id)
	require.ErrorIs(t, err, ErrReportRevisionRequired)
	old, err := ops.GetEmailNotificationConfig(ctx)
	require.NoError(t, err)
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":true,"monthly_usage_enabled":false,"monthly_usage_department_attribute_id":23,"recipients":["report@example.org"]}}`))
	req, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err = ops.StartManualMonthlyUsage(ctx, "2024-02", id, old.ReportRevision)
	require.ErrorIs(t, err, ErrReportRevisionConflict)
	require.Empty(t, store.jobs)
	cfg, err := ops.GetEmailNotificationConfig(ctx)
	require.NoError(t, err)
	store.onInsert = cancel // Browser disconnects while the INSERT result is pending.
	first, err := ops.StartManualMonthlyUsage(req, "2024-02", id, cfg.ReportRevision)
	require.NoError(t, err)
	require.Equal(t, "running", first.Status)
	require.ErrorIs(t, req.Err(), context.Canceled)
	select {
	case <-store.started:
	case <-time.After(3 * time.Second):
		t.Fatal("export did not start")
	}
	same, err := ops.StartManualMonthlyUsage(ctx, "2024-02", id)
	require.NoError(t, err)
	require.Equal(t, id, same.ID)
	_, err = ops.StartManualMonthlyUsage(ctx, "2024-01", id)
	require.ErrorIs(t, err, ErrMonthlyUsageConflict)
	_, err = ops.StartManualMonthlyUsage(ctx, "2024-02", uuid.NewString(), cfg.ReportRevision)
	require.ErrorIs(t, err, ErrMonthlyUsageBusy)
	close(store.proceed)
	worker.wg.Wait()
	completed, err := ops.ManualMonthlyUsageStatus(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "sent", completed.Status)
	require.Equal(t, 1, completed.Accepted)
	_, err = ops.StartManualMonthlyUsage(ctx, "2024-02", id, old.ReportRevision)
	require.NoError(t, err)
	require.Equal(t, int64(1), smtp.messageCount())
	_, err = ops.StartManualMonthlyUsage(ctx, "2024-02", uuid.NewString(), cfg.ReportRevision) // deliberate resend
	require.NoError(t, err)
	worker.wg.Wait()
	require.Equal(t, int64(2), smtp.messageCount())
	require.Equal(t, []int64{23, 23}, store.attributeIDs)
	_, err = settings.GetValue(ctx, monthlyUsageCheckpoint)
	require.ErrorIs(t, err, ErrSettingNotFound)
	require.Empty(t, store.states) // manual send has independent receipts
}

func TestManualMonthlyUsagePartialOutcomesAndRemovedRecipient(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":true,"recipients":["yes@example.org","unknown@example.org","new@example.org"]}}`))
	store := &manualMonthlyStoreStub{monthlyTestStore: &monthlyTestStore{}, jobs: map[string]OpsMonthlyUsageJob{}}
	worker := &OpsScheduledReportService{opsService: &OpsService{settingRepo: settings, opsRepo: store}}
	job := OpsMonthlyUsageJob{ID: uuid.NewString(), Month: "2024-02", Total: 3, Status: "running"}
	var sent []string
	worker.runManualMonthlyUsage(ctx, store, store, job, []string{"yes@example.org", "removed@example.org", "unknown@example.org"}, 0, func(_ context.Context, to, _, body, _ string, _ []byte) (bool, error) {
		require.Contains(t, body, "本月非管理员已入账请求为 0。")
		sent = append(sent, to)
		if to == "unknown@example.org" {
			return true, errors.New("no receipt")
		}
		return false, nil
	})
	got, err := store.GetManualMonthlyUsage(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"yes@example.org", "unknown@example.org"}, sent)
	require.Equal(t, "uncertain", got.Status)
	require.Equal(t, 1, got.Accepted)
	require.Equal(t, 1, got.Failed)
	require.Equal(t, 1, got.Uncertain)
}

func TestManualMonthlyUsageBeijingMonthBoundary(t *testing.T) {
	now := time.Date(2024, 2, 29, 16, 0, 0, 0, time.UTC)
	require.NoError(t, validateManualMonthlyPeriod("2024-02", now))
	for _, month := range []string{"", "0000-01", "2024-2", "2024-03", "2025-01"} {
		require.ErrorIs(t, validateManualMonthlyPeriod(month, now), ErrMonthlyUsageInvalid)
	}
	require.ErrorIs(t, validateManualMonthlyPeriod("2024-02", now.Add(-time.Second)), ErrMonthlyUsageInvalid)
}

func TestManualMonthlyUsageRechecksApprovalBeforeEachMessage(t *testing.T) {
	const report = `{"report":{"enabled":true,"recipients":["first@example.org","second@example.org"]}}`
	for _, tc := range []struct{ name, key, value string }{
		{"recipient replaced", SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":true,"recipients":["first@example.org","new@example.org"]}}`},
		{"reports disabled", SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":false,"recipients":["second@example.org"]}}`},
		{"department changed", SettingKeyOpsEmailNotificationConfig, `{"report":{"enabled":true,"monthly_usage_department_attribute_id":23,"recipients":["second@example.org"]}}`},
		{"monitoring disabled", SettingKeyOpsMonitoringEnabled, "false"},
		{"unreadable approval", SettingKeyOpsEmailNotificationConfig, "{"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			settings := newNotificationEmailMemorySettingRepo()
			require.NoError(t, settings.Set(ctx, SettingKeyOpsEmailNotificationConfig, report))
			store := &manualMonthlyStoreStub{monthlyTestStore: &monthlyTestStore{}, jobs: map[string]OpsMonthlyUsageJob{}}
			worker := &OpsScheduledReportService{opsService: &OpsService{settingRepo: settings, opsRepo: store}}
			job := OpsMonthlyUsageJob{ID: uuid.NewString(), Month: "2024-02", Total: 2, Status: "running"}
			var sent []string
			worker.runManualMonthlyUsage(ctx, store, store, job, []string{"first@example.org", "second@example.org"}, 0,
				func(_ context.Context, to, _, _, _ string, _ []byte) (bool, error) {
					sent = append(sent, to)
					require.NoError(t, settings.Set(ctx, tc.key, tc.value))
					return false, nil
				})
			require.Equal(t, []string{"first@example.org"}, sent)
			got, err := store.GetManualMonthlyUsage(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", got.Status)
			require.Equal(t, 1, got.Failed)
			require.Equal(t, 1, got.Accepted)
		})
	}
}
