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

func TestUsageRequestPhaseAveragesRespectFiltersAndMissingSamples(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	u := mustCreateUser(t, client, &service.User{Email: "request-phases@synthetic.invalid"})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "request-phases", Name: "phases"})
	a := mustCreateAccount(t, client, &service.Account{Name: "phases", Platform: service.PlatformOpenAI})
	now := time.Now().UTC().Add(-time.Minute)
	start, end := now.Add(-time.Minute), now.Add(time.Minute)
	filters := UsageLogFilters{UserID: u.ID, APIKeyID: k.ID, Model: "public-model", ModelFilterSource: "requested", StartTime: &start, EndTime: &end}
	add := func(upload, processing float64, model string, at time.Time, traceCount int, incomplete bool) int64 {
		t.Helper()
		usage := &service.UsageLog{UserID: u.ID, APIKeyID: k.ID, AccountID: a.ID, Model: "upstream-model", RequestedModel: model, CreatedAt: at}
		_, err := repo.Create(ctx, usage)
		require.NoError(t, err)
		for i := 0; i < traceCount; i++ {
			s := requesttiming.Snapshot{BodyComplete: !incomplete, TotalMS: 100 + upload + processing, Events: map[string]float64{"body_read_start": 100, "body_received": 100 + upload}}
			raw, err := json.Marshal(s)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, usage.ID, uuid.NewString(), string(raw))
			require.NoError(t, err)
		}
		return usage.ID
	}
	first := add(5000, 90000, "public-model", now, 1, false)
	add(0, 10000, "public-model", now, 1, false)
	add(60000, 60000, "public-model", now, 0, false) // historical/expired
	add(60000, 60000, "public-model", now, 2, false) // ambiguous billing retry
	add(60000, 60000, "public-model", now, 1, true)  // incomplete upload
	add(60000, 60000, "other-model", now, 1, false)
	add(60000, 60000, "public-model", end, 1, false) // end is exclusive
	for _, raw := range []string{
		`{"body_complete":true,"total_ms":100,"events":{"body_read_start":10,"body_received":5}}`,
		`{"body_complete":true,"total_ms":100,"events":{"body_read_start":0,"body_received":200}}`,
		`{"body_complete":true,"total_ms":100,"events":{"body_read_start":0,"body_received":5,"body_read_error":5}}`,
		`{"body_complete":true,"total_ms":100,"events":{"body_read_start":"bad","body_received":5}}`,
		`{}`,
	} {
		id := add(0, 0, "public-model", now, 0, false)
		_, err := tx.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3::jsonb)`, id, uuid.NewString(), raw)
		require.NoError(t, err)
	}
	stats, err := repo.ReadUsageRequestPhaseAverages(ctx, filters)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.TimingSampleCount)
	require.Equal(t, 2500.0, *stats.AverageUserUploadMs)
	require.Equal(t, 50000.0, *stats.AverageProcessingMs)
	items, err := repo.ReadUsageContentTimings(ctx, []int64{first})
	require.NoError(t, err)
	require.Equal(t, 5000.0, *items[first].RequestPhases.UserUploadMS)
	require.Equal(t, 90000.0, *items[first].RequestPhases.ProcessingMS)
	filters.UserID = u.ID + 10000
	empty, err := repo.ReadUsageRequestPhaseAverages(ctx, filters)
	require.NoError(t, err)
	require.Zero(t, empty.TimingSampleCount)
	require.Nil(t, empty.AverageUserUploadMs)
	require.Nil(t, empty.AverageProcessingMs)
}
