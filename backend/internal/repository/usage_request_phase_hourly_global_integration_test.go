//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func addGlobalPhaseTestUsage(t *testing.T, ctx context.Context, repo *usageLogRepository, userID, keyID, accountID int64, at time.Time) int64 {
	t.Helper()
	row := &service.UsageLog{UserID: userID, APIKeyID: keyID, AccountID: accountID, Model: "phase-hour-test", CreatedAt: at}
	_, err := repo.Create(ctx, row)
	require.NoError(t, err)
	return row.ID
}

func addGlobalPhaseTestDetail(t *testing.T, ctx context.Context, exec sqlExecutor, usageID int64, upload, processing float64, complete bool) string {
	t.Helper()
	trace := uuid.NewString()
	snapshot := requesttiming.Snapshot{
		BodyComplete: complete,
		TotalMS:      100 + upload + processing,
		Events:       map[string]float64{"body_read_start": 100, "body_received": 100 + upload},
	}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	_, err = exec.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, usageID, trace, string(raw))
	require.NoError(t, err)
	return trace
}

func TestGlobalRequestPhaseHourlyMatchesExactRollingWindow(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newUsageLogRepositoryWithSQL(tx.Client(), tx)
	u := mustCreateUser(t, tx.Client(), &service.User{Email: "phase-hour-window@synthetic.invalid"})
	k := mustCreateApiKey(t, tx.Client(), &service.APIKey{UserID: u.ID, Key: "phase-hour-window", Name: "phase-hour-window"})
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "phase-hour-window", Platform: service.PlatformOpenAI})
	endHour := time.Now().UTC().Truncate(time.Hour).Add(72 * time.Hour)
	end := endHour.Add(30 * time.Minute)
	start := end.Add(-24 * time.Hour)
	firstFull := start.Truncate(time.Hour).Add(time.Hour)

	_, err := repo.ReadGlobalRequestPhaseHourlyAverages(ctx, start, end)
	require.ErrorContains(t, err, "buckets incomplete")
	add := func(at time.Time, upload, processing float64, complete bool) int64 {
		id := addGlobalPhaseTestUsage(t, ctx, repo, u.ID, k.ID, a.ID, at)
		addGlobalPhaseTestDetail(t, ctx, tx, id, upload, processing, complete)
		return id
	}
	first := add(start, 100, 900, true) // first partial hour, inclusive
	middle := add(firstFull.Add(15*time.Minute), 200, 800, true)
	last := add(endHour.Add(15*time.Minute), 300, 700, true) // last partial hour
	add(start.Add(-time.Second), 10000, 10000, true)         // before window
	add(end, 10000, 10000, true)                             // exclusive end
	add(firstFull.Add(20*time.Minute), 10000, 10000, false)
	ambiguous := add(firstFull.Add(25*time.Minute), 10000, 10000, true)
	addGlobalPhaseTestDetail(t, ctx, tx, ambiguous, 10000, 10000, true)
	late := addGlobalPhaseTestUsage(t, ctx, repo, u.ID, k.ID, a.ID, firstFull.Add(30*time.Minute))

	for hour := firstFull; hour.Before(endHour); hour = hour.Add(time.Hour) {
		require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, hour))
	}
	alignedStart := endHour.Add(-24 * time.Hour)
	require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, alignedStart))
	aligned, err := repo.ReadGlobalRequestPhaseHourlyAverages(ctx, alignedStart, endHour)
	require.NoError(t, err)
	alignedDirect, err := repo.ReadUsageRequestPhaseAverages(ctx, UsageLogFilters{StartTime: &alignedStart, EndTime: &endHour})
	require.NoError(t, err)
	require.Equal(t, alignedDirect, aligned, "an exactly aligned window must use all 24 buckets")
	assertMatches := func(samples int64, upload, processing float64) {
		t.Helper()
		got, err := repo.ReadGlobalRequestPhaseHourlyAverages(ctx, start, end)
		require.NoError(t, err)
		direct, err := repo.ReadUsageRequestPhaseAverages(ctx, UsageLogFilters{StartTime: &start, EndTime: &end})
		require.NoError(t, err)
		require.Equal(t, direct, got)
		require.Equal(t, samples, got.TimingSampleCount)
		if samples == 0 {
			require.Nil(t, got.AverageUserUploadMs)
			require.Nil(t, got.AverageProcessingMs)
		} else {
			require.InDelta(t, upload, *got.AverageUserUploadMs, 0.001)
			require.InDelta(t, processing, *got.AverageProcessingMs, 0.001)
		}
	}
	assertMatches(3, 200, 800)
	lateTrace := addGlobalPhaseTestDetail(t, ctx, tx, late, 400, 600, true)
	require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, firstFull))
	assertMatches(4, 250, 750)
	duplicate := addGlobalPhaseTestDetail(t, ctx, tx, late, 400, 600, true)
	require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, firstFull))
	assertMatches(3, 200, 800)
	_, err = tx.ExecContext(ctx, `DELETE FROM request_timing_details WHERE usage_log_id=$1 AND trace_id=$2`, late, duplicate)
	require.NoError(t, err)
	require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, firstFull))
	assertMatches(4, 250, 750)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE id=$1`, late)
	require.NoError(t, err)
	require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, firstFull))
	assertMatches(3, 200, 800)
	_, err = tx.ExecContext(ctx, `DELETE FROM request_timing_details WHERE usage_log_id IN ($1,$2,$3,$4)`, first, middle, last, ambiguous)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM request_timing_details WHERE usage_log_id=$1 AND trace_id=$2`, late, lateTrace)
	require.NoError(t, err)
	require.NoError(t, recomputeGlobalRequestPhaseHour(ctx, tx, firstFull))
	assertMatches(0, 0, 0)
}

func TestGlobalRequestPhaseDirtyTriggerTracksLateChanges(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newUsageLogRepositoryWithSQL(tx.Client(), tx)
	u := mustCreateUser(t, tx.Client(), &service.User{Email: "phase-hour-trigger@synthetic.invalid"})
	k := mustCreateApiKey(t, tx.Client(), &service.APIKey{UserID: u.ID, Key: "phase-hour-trigger", Name: "phase-hour-trigger"})
	a := mustCreateAccount(t, tx.Client(), &service.Account{Name: "phase-hour-trigger", Platform: service.PlatformOpenAI})
	hour := time.Now().UTC().Truncate(time.Hour).Add(-16 * time.Hour)
	next := hour.Add(time.Hour)
	_, err := tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start IN ($1,$2)`, hour, next)
	require.NoError(t, err)
	id := addGlobalPhaseTestUsage(t, ctx, repo, u.ID, k.ID, a.ID, hour.Add(5*time.Minute))
	var count int
	require.NoError(t, scanSingleRow(ctx, tx, `SELECT COUNT(*) FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, []any{hour}, &count))
	require.Equal(t, 1, count, "a backdated usage insert must mark its completed hour")
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, hour)
	require.NoError(t, err)
	trace := addGlobalPhaseTestDetail(t, ctx, tx, id, 100, 900, true)
	assertDirty := func(want ...time.Time) {
		t.Helper()
		for _, at := range want {
			var count int
			require.NoError(t, scanSingleRow(ctx, tx, `SELECT COUNT(*) FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, []any{at}, &count))
			require.Equal(t, 1, count)
		}
	}
	assertDirty(hour)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, hour)
	require.NoError(t, err)
	duplicate := addGlobalPhaseTestDetail(t, ctx, tx, id, 100, 900, true)
	assertDirty(hour)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, hour)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM request_timing_details WHERE usage_log_id=$1 AND trace_id=$2`, id, duplicate)
	require.NoError(t, err)
	assertDirty(hour)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, hour)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE request_timing_details SET detail=jsonb_set(detail,'{total_ms}','1100') WHERE usage_log_id=$1 AND trace_id=$2`, id, trace)
	require.NoError(t, err)
	assertDirty(hour)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, hour)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE usage_logs SET created_at=$2 WHERE id=$1`, id, next.Add(5*time.Minute))
	require.NoError(t, err)
	assertDirty(hour, next)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start IN ($1,$2)`, hour, next)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE id=$1`, id)
	require.NoError(t, err)
	assertDirty(next)
	current := time.Now().UTC().Truncate(time.Hour)
	_, err = tx.ExecContext(ctx, `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, current)
	require.NoError(t, err)
	currentID := addGlobalPhaseTestUsage(t, ctx, repo, u.ID, k.ID, a.ID, current.Add(5*time.Minute))
	addGlobalPhaseTestDetail(t, ctx, tx, currentID, 100, 900, true)
	var currentDirty int
	require.NoError(t, scanSingleRow(ctx, tx, `SELECT COUNT(*) FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, []any{current}, &currentDirty))
	require.Zero(t, currentDirty, "ordinary writes in the current hour must not contend on a dirty row")
}

func TestGlobalRequestPhaseSeedKeeps24CompletedHours(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newUsageLogRepositoryWithSQL(tx.Client(), tx)
	now := time.Now().UTC().Truncate(time.Hour).Add(120 * time.Hour)
	for i := 0; i < 2; i++ {
		require.NoError(t, repo.SeedGlobalRequestPhaseHourBuckets(ctx, now))
		var count int
		require.NoError(t, scanSingleRow(ctx, tx, `SELECT COUNT(*) FROM usage_request_phase_hourly_dirty WHERE bucket_start >= $1 AND bucket_start < $2`,
			[]any{now.Add(-24 * time.Hour), now}, &count))
		require.Equal(t, 24, count)
	}
}

func TestGlobalRequestPhaseWorkerRefreshIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := newUsageLogRepositoryWithSQL(integrationEntClient, integrationDB)
	now := time.Now().UTC().Truncate(time.Hour).Add(96 * time.Hour)
	bucket := now.Add(-time.Hour)
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO usage_request_phase_hourly_dirty(bucket_start) VALUES($1) ON CONFLICT(bucket_start) DO NOTHING`, bucket)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM usage_request_phase_hourly_dirty WHERE bucket_start=$1`, bucket)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM usage_request_phase_hourly_global WHERE bucket_start=$1`, bucket)
	})
	updated, err := repo.RefreshOneGlobalRequestPhaseHourBucket(ctx, now)
	require.NoError(t, err)
	require.True(t, updated)
	updated, err = repo.RefreshOneGlobalRequestPhaseHourBucket(ctx, now)
	require.NoError(t, err)
	require.False(t, updated)
	var samples int64
	require.NoError(t, scanSingleRow(ctx, integrationDB, `SELECT sample_count FROM usage_request_phase_hourly_global WHERE bucket_start=$1`, []any{bucket}, &samples))
	require.Zero(t, samples)
}
