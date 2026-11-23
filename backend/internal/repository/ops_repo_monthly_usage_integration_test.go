//go:build integration

package repository

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/monthlyusage"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMonthlyUsageSnapshotAndWorkbook(t *testing.T) {
	ctx := context.Background()
	schema := "monthly_report_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := integrationDB.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
	db := openGroupUsageRollupSchemaDB(t, schema, "America/Los_Angeles")

	// The department encodes transaction settings and start time. Its value must
	// match across both queries, including when an empty model falls back.
	_, err = db.ExecContext(ctx, `
CREATE TABLE usage_dashboard_daily (bucket_date date PRIMARY KEY, total_requests bigint);
CREATE TABLE usage_group_rollup_state (id int PRIMARY KEY, expired_before timestamptz);
INSERT INTO usage_group_rollup_state VALUES (1, '1970-01-01Z');
CREATE TABLE usage_cleanup_tasks (status text, started_at timestamptz, deleted_rows bigint, filters jsonb);
CREATE VIEW settings AS SELECT key, value FROM public.settings WHERE false;
CREATE VIEW security_penalty_subjects AS SELECT * FROM public.security_penalty_subjects WHERE false;
CREATE VIEW groups AS SELECT 9 AS id, 'old group' AS name;
CREATE VIEW users AS SELECT id, 'email@example.invalid' AS username, '2024-01-01'::timestamptz AS created_at,
    deleted_at, role, 'active' AS status
    FROM (VALUES (1, 'user', NULL::timestamptz), (2, 'admin', NULL),
                 (3, 'admin', NULL), (4, 'admin', '2024-03-15Z')) u(id, role, deleted_at);
CREATE TABLE user_attribute_definitions (
    id bigint PRIMARY KEY, key text, type text, options jsonb, enabled boolean DEFAULT true, deleted_at timestamptz
);
INSERT INTO user_attribute_definitions (id, key, type) VALUES (1, 'department', 'text');
CREATE VIEW user_attribute_values AS SELECT id AS user_id, 1 AS attribute_id,
    CASE WHEN id = 3 THEN 'admin-only' ELSE
        concat_ws('/', current_setting('transaction_read_only'), current_setting('transaction_isolation'), now()::text)
    END AS value FROM users;
CREATE VIEW usage_logs AS SELECT id AS user_id, ' '::text AS requested_model, 'fixture-model' AS model,
    9 AS group_id, 21 AS input_tokens, 3 AS output_tokens, 5 AS cache_creation_tokens, 7 AS cache_read_tokens,
    0.1000000001::numeric AS actual_cost, ts AS created_at
    FROM (VALUES ('2024-01-31 16:00Z'::timestamptz), ('2024-02-29 16:00Z'::timestamptz),
                 ('2024-04-30 16:00Z'::timestamptz)) days(ts) CROSS JOIN users;`)
	require.NoError(t, err)
	repo := NewOpsRepository(db).(*opsRepository)
	var snapshot bytes.Buffer
	meta, err := repo.snapshotMonthlyUsage(ctx, "2024-02-01", 0, &snapshot)
	require.NoError(t, err)
	decoder := gob.NewDecoder(&snapshot)
	var row monthlyusage.Row
	require.NoError(t, decoder.Decode(&row))
	require.True(t, strings.HasPrefix(row.Department, "on/repeatable read/"))
	require.Equal(t, map[string]int64{row.Department: 1}, meta.registered)
	require.Equal(t, monthlyusage.Row{Day: "2024-02-01", UserID: "1", Department: row.Department,
		Groups: "old group", Model: "fixture-model", Requests: "1", InputTokens: "21", OutputTokens: "3",
		CacheReadTokens: "7", TotalTokens: "36", ActualCost: "0.1000000001"}, row)
	require.ErrorIs(t, decoder.Decode(&row), io.EOF)

	var out bytes.Buffer
	w := monthlyReportWriter(func(p []byte) (int, error) {
		require.Zero(t, db.Stats().InUse, "output must not hold a database snapshot")
		return out.Write(p)
	})
	require.NoError(t, repo.BuildMonthlyUsageReport(ctx, "2024-02", "boscserver", 0, w))

	// An explicit field with an empty or missing value must ignore the populated legacy department.
	_, err = db.ExecContext(ctx, `
INSERT INTO user_attribute_definitions (id, key, type, options) VALUES
    (2, 'team', 'select', '[{"value":" eng ","label":" 研发 "}]'),
    (3, 'notes', 'textarea', '[{"value":"  研发  ","label":"unused"}]'),
    (4, 'empty_team', 'select', '[]'),
    (5, 'past_team', 'select', '[]'),
    (6, 'missing_value', 'text', NULL),
    (7, 'members', 'multi_select', '[]');
CREATE OR REPLACE VIEW user_attribute_values AS
    SELECT 1 AS user_id, 1 AS attribute_id, 'legacy department' AS value
    UNION ALL SELECT 1, 2, ' eng '
    UNION ALL SELECT 2, 2, ' eng '
    UNION ALL SELECT 3, 2, 'admin-only'
    UNION ALL SELECT 4, 2, 'deleted-admin'
    UNION ALL SELECT 1, 3, '  研发  '
    UNION ALL SELECT 1, 4, E' \t'
    UNION ALL SELECT 1, 5, ' retired-option ';`)
	require.NoError(t, err)
	for _, tc := range []struct {
		id         int64
		department string
	}{
		{2, "研发"}, {3, "研发"}, {4, ""}, {5, "retired-option"}, {6, ""},
	} {
		ok, err := repo.MonthlyUsageDepartmentAttributeAvailable(ctx, tc.id)
		require.NoError(t, err)
		require.True(t, ok)
		snapshot.Reset()
		meta, err := repo.snapshotMonthlyUsage(ctx, "2024-02-01", tc.id, &snapshot)
		require.NoError(t, err)
		var detail monthlyusage.Row
		require.NoError(t, gob.NewDecoder(&snapshot).Decode(&detail))
		require.Equal(t, tc.department, detail.Department, "attribute %d", tc.id)
		require.Equal(t, map[string]int64{tc.department: 1}, meta.registered)
	}

	// The dashboard is configured in UTC and the DB session is in Los Angeles.
	// Jan 31 includes Feb 1 Beijing usage from one ordinary user and three admins.
	_, err = db.ExecContext(ctx, `INSERT INTO usage_dashboard_daily VALUES ('2024-01-31', 4)`)
	require.NoError(t, err)
	notice, err := repo.BuildMonthlyUsageReportWithCoverage(ctx, "2024-02", "boscserver", 2, io.Discard)
	require.NoError(t, err)
	require.NotContains(t, notice, "历史覆盖无法确认")
	_, err = db.ExecContext(ctx, `UPDATE usage_dashboard_daily SET total_requests = 6`)
	require.NoError(t, err)
	notice, err = repo.BuildMonthlyUsageReportWithCoverage(ctx, "2024-02", "boscserver", 2, io.Discard)
	require.NoError(t, err)
	require.Contains(t, notice, "2024-01-31（UTC）日汇总全站 6 笔，现存 4 笔，该日跨北京时间月界")

	// Daily aggregates include admins. Only compare against the unfiltered ledger.
	_, err = db.ExecContext(ctx, `INSERT INTO usage_dashboard_daily VALUES ('2024-02-10', 83)`)
	require.NoError(t, err)
	out.Reset()
	notice, err = repo.BuildMonthlyUsageReportWithCoverage(ctx, "2024-02", "boscserver", 2, w)
	require.NoError(t, err)
	require.Contains(t, notice, "2024-02-10（UTC）日汇总全站 83 笔，现存 0 笔")
	book, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	require.NoError(t, err)
	for _, sheet := range []string{"sheet1", "sheet2", "sheet3"} {
		part, err := book.Open("xl/worksheets/" + sheet + ".xml")
		require.NoError(t, err)
		body, err := io.ReadAll(part)
		require.NoError(t, part.Close())
		require.NoError(t, err)
		require.Equal(t, 3, bytes.Count(body, []byte("<row ")), "worksheet %s", sheet) // coverage, header and one non-admin row
		require.Contains(t, string(body), notice, "worksheet %s", sheet)
		require.Contains(t, string(body), "研发", "worksheet %s", sheet)
		require.NotContains(t, string(body), "admin-only", "worksheet %s", sheet)
	}

	// April is bracketed by site history but has no requests of its own.
	checkApril := func(expected string) {
		t.Helper()
		out.Reset()
		notice, err := repo.BuildMonthlyUsageReportWithCoverage(ctx, "2024-04", "boscserver", 0, w)
		require.NoError(t, err)
		require.Contains(t, notice, expected)
	}
	update := func(query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	update(`UPDATE usage_group_rollup_state SET expired_before = '2024-03-31 16:00Z'`)
	checkApril("已入账请求为 0")
	update(`UPDATE usage_group_rollup_state SET expired_before = '2024-03-31 16:00:01Z'`)
	checkApril("不能认定为零请求")
	update(`UPDATE usage_group_rollup_state SET expired_before = '1970-01-01Z';
INSERT INTO usage_cleanup_tasks VALUES ('succeeded', now(), 1,
    '{"start_time":"2024-04-30T16:00:00Z","end_time":"2024-05-09T12:00:00Z"}')`)
	checkApril("已入账请求为 0") // the cleanup starts at the next month's boundary
	update(`UPDATE usage_cleanup_tasks SET filters = '{"start_time":"2024-03-19T12:00:00Z","end_time":"2024-03-31T16:00:00Z"}'`)
	checkApril("清理任务涉及本月") // cleanup's inclusive end touches the month
	update(`DELETE FROM usage_cleanup_tasks; DELETE FROM usage_group_rollup_state`)
	checkApril("缺少日志保留边界")

	_, err = db.ExecContext(ctx, `
UPDATE user_attribute_definitions SET enabled = false WHERE id = 2;
UPDATE user_attribute_definitions SET deleted_at = now() WHERE id = 3;
INSERT INTO user_attribute_definitions (id, key, type) VALUES (8, 'notes', 'textarea');`)
	require.NoError(t, err)
	for _, id := range []int64{2, 3, 7, 999, -1} {
		ok, err := repo.MonthlyUsageDepartmentAttributeAvailable(ctx, id)
		require.NoError(t, err)
		require.False(t, ok)
		out.Reset()
		require.ErrorContains(t, repo.BuildMonthlyUsageReport(ctx, "2024-02", "boscserver", id, &out), "unavailable")
		require.Zero(t, out.Len())
	}
}

type monthlyReportWriter func([]byte) (int, error)

func (f monthlyReportWriter) Write(p []byte) (int, error) { return f(p) }

func TestMonthlyUsageDeliveryClaims(t *testing.T) {
	ctx := context.Background()
	addr := uuid.NewString() + "@example.invalid"
	other := "other-" + addr
	const month = "2024-02"
	t.Cleanup(func() {
		for _, recipient := range []string{addr, other} {
			_, hash, err := monthlyDeliveryKey(month, recipient)
			require.NoError(t, err)
			_, err = integrationDB.Exec("DELETE FROM monthly_usage_email_deliveries WHERE recipient_hash=$1", hash)
			require.NoError(t, err)
		}
	})

	// Independent pools represent two processes competing for the same send.
	db, err := sql.Open("postgres", integrationDSN)
	require.NoError(t, err)
	defer db.Close()
	repos := []*opsRepository{{db: db}, {db: integrationDB}}
	checkState := func(repo *opsRepository, month, recipient string, expected service.MonthlyUsageDeliveryState) {
		t.Helper()
		got, err := repo.MonthlyUsageDelivery(ctx, month, recipient)
		require.NoError(t, err)
		require.Equal(t, expected, got)
	}
	for _, repo := range repos {
		checkState(repo, month, addr, service.MonthlyUsageUnclaimed)
	}
	const workers = 16
	var wg sync.WaitGroup
	var claims [workers]bool
	var errs [workers]error
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claims[i], errs[i] = repos[i%2].ClaimMonthlyUsageEmail(ctx, month, addr)
		}()
	}
	wg.Wait()
	winners := 0
	for i := range workers {
		require.NoError(t, errs[i])
		if claims[i] {
			winners++
		}
	}
	require.Equal(t, 1, winners)
	checkState(repos[0], month, addr, service.MonthlyUsageSending)
	require.NoError(t, db.Close())

	r := NewOpsRepository(integrationDB).(*opsRepository)
	claim := func(month, addr string, expected bool) {
		t.Helper()
		got, err := r.ClaimMonthlyUsageEmail(ctx, month, addr)
		require.NoError(t, err)
		require.Equal(t, expected, got)
	}
	canonicalVariant := " " + strings.ToUpper(addr) + " "
	checkState(r, month, canonicalVariant, service.MonthlyUsageSending)
	claim(month, canonicalVariant, false) // pending/unknown survives restart
	checkState(r, "2024-03", addr, service.MonthlyUsageUnclaimed)
	checkState(r, month, other, service.MonthlyUsageUnclaimed)
	claim("2024-03", addr, true)
	claim(month, other, true)
	require.NoError(t, r.FinishMonthlyUsageEmail(ctx, month, addr, service.MonthlyUsageRejected))
	checkState(r, month, addr, service.MonthlyUsageRejected)
	checkState(r, "2024-03", addr, service.MonthlyUsageSending)
	checkState(r, month, other, service.MonthlyUsageSending)
	retry, err := r.NextMonthlyUsageRetry(ctx, month, "2024-03", []string{other})
	require.NoError(t, err)
	require.Empty(t, retry)
	retry, err = r.NextMonthlyUsageRetry(ctx, month, "2024-03", []string{canonicalVariant})
	require.NoError(t, err)
	require.Equal(t, month, retry)
	claim(month, addr, true)
	checkState(r, month, addr, service.MonthlyUsageSending)
	require.NoError(t, r.FinishMonthlyUsageEmail(ctx, month, addr, service.MonthlyUsageAccepted))
	checkState(r, month, addr, service.MonthlyUsageAccepted)
	require.Error(t, r.FinishMonthlyUsageEmail(ctx, month, addr, service.MonthlyUsageRejected))
	checkState(r, month, addr, service.MonthlyUsageAccepted)
	claim(month, addr, false)

	// The worker has at most 15 minutes. Keep a pending send from advancing the
	// checkpoint during the full 30-minute grace period, even after a restart.
	date, hash, err := monthlyDeliveryKey(month, other)
	require.NoError(t, err)
	setAge := func(minutes int) {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, `UPDATE monthly_usage_email_deliveries
SET claimed_at = now() - make_interval(mins => $3) WHERE report_month=$1 AND recipient_hash=$2`, date, hash, minutes)
		require.NoError(t, err)
	}
	setAge(29)
	checkState(r, month, other, service.MonthlyUsageSending)
	setAge(31)
	checkState(r, month, other, service.MonthlyUsageUncertain)
	claim(month, other, false) // unknown receipt remains ineligible for resend
}

func TestMonthlyUsageDeliveryMigration389Replay(t *testing.T) {
	ctx := context.Background()
	schema := "monthly_delivery_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec("DROP SCHEMA " + schema + " CASCADE")
		require.NoError(t, err)
	})
	db := openGroupUsageRollupSchemaDB(t, schema, "UTC")
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err)
	}
	apply := func(name string) {
		t.Helper()
		script, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		exec(string(script))
	}
	apply("388_monthly_usage_email_deliveries.sql")
	exec("CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL, updated_at timestamptz DEFAULT now())")
	exec("INSERT INTO settings (key,value) VALUES ($1,$2),($3,$4)", "ops_monthly_usage_last_completed_period", "2024-01", service.SettingKeyOpsEmailNotificationConfig, "saved")
	const month, addr = "2024-02", "old@example.org"
	date, hash, err := monthlyDeliveryKey(month, addr)
	require.NoError(t, err)
	exec("INSERT INTO monthly_usage_email_deliveries(report_month,recipient_hash,claimed_at) VALUES ($1,$2,now()-interval '1 day')", date, hash)
	const migration = "389_monthly_usage_delivery_outcomes.sql"
	apply(migration)
	repo := NewOpsRepository(db).(*opsRepository)
	cursor := func() string {
		t.Helper()
		var value string
		require.NoError(t, db.QueryRow("SELECT value FROM settings WHERE key=$1", service.MonthlyUsageDispatchCheckpoint).Scan(&value))
		return value
	}
	require.Equal(t, "2024-01", cursor())
	advanced, err := repo.AdvanceMonthlyUsageDispatch(ctx, month, "obsolete")
	require.NoError(t, err)
	require.False(t, advanced)
	advanced, err = repo.AdvanceMonthlyUsageDispatch(ctx, month, "saved")
	require.NoError(t, err)
	require.True(t, advanced)
	apply(migration)
	require.Equal(t, month, cursor())
	advanced, err = repo.AdvanceMonthlyUsageDispatch(ctx, "2024-01", "saved")
	require.NoError(t, err)
	require.False(t, advanced)
	state, err := repo.MonthlyUsageDelivery(ctx, month, addr)
	require.NoError(t, err)
	require.Equal(t, service.MonthlyUsageUncertain, state)
	claimed, err := repo.ClaimMonthlyUsageEmailForConfig(ctx, month, addr, "saved")
	require.NoError(t, err)
	require.False(t, claimed)
	claimed, err = repo.ClaimMonthlyUsageEmailForConfig(ctx, month, "other@example.org", "obsolete")
	require.NoError(t, err)
	require.False(t, claimed)
	claimed, err = repo.ClaimMonthlyUsageEmailForConfig(ctx, month, "other@example.org", "saved")
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, repo.FinishMonthlyUsageEmail(ctx, month, "other@example.org", service.MonthlyUsageUncertain))
	state, err = repo.MonthlyUsageDelivery(ctx, month, "other@example.org")
	require.NoError(t, err)
	require.Equal(t, service.MonthlyUsageUncertain, state)
	var indexExists bool
	require.NoError(t, db.QueryRow("SELECT to_regclass('monthly_usage_rejected_retry') IS NOT NULL").Scan(&indexExists))
	require.True(t, indexExists)
	_, err = db.Exec("UPDATE monthly_usage_email_deliveries SET accepted_at=now(),rejected_at=now()")
	require.ErrorContains(t, err, "monthly_usage_one_delivery_outcome")
}
