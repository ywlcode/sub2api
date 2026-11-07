//go:build retentionintegration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil/retentionpg"
	"github.com/stretchr/testify/require"
)

func TestRetentionPostgresMigrations(t *testing.T) {
	db := retentionpg.Open(t)
	ctx := context.Background()
	require.NoError(t, ApplyMigrations(ctx, db))
	require.NoError(t, ApplyMigrations(ctx, db), "startup migration replay must be safe")
	require.EqualValues(t, 2, retentionpg.Count(t, db, `SELECT count(*) FROM pg_index i
	 JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indisvalid AND i.indisready
	 AND c.relname IN ('usage_billing_events_completed_retention_idx','usage_billing_dedup_archive_created_at_brin')`))
}

// Invoked by the runner after the actual cleanup worker has processed the
// synthetic public dataset, and again after restoring its compressed backup.
func TestRetentionPostgresBillingReplay(t *testing.T) {
	db := retentionpg.Open(t)
	repo := &usageBillingRepository{db: db}
	ctx := context.Background()
	for _, request := range []string{"retention-hot-replay", "retention-archive-replay"} {
		cmd := &service.UsageBillingCommand{RequestID: request, APIKeyID: 910001,
			UserID: 910001, BalanceCost: 1, RequestFingerprint: "retention-fixture-fingerprint"}
		for i := 0; i < 3; i++ {
			result, err := repo.Apply(ctx, cmd)
			require.NoError(t, err)
			require.False(t, result.Applied, "replay must not debit balance after cleanup/restore")
		}
		cmd.RequestFingerprint = "different-fingerprint"
		_, err := repo.Apply(ctx, cmd)
		require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
	}
	require.EqualValues(t, 100, retentionpg.Count(t, db, `SELECT balance::bigint FROM users WHERE id=910001`))
	require.Zero(t, retentionpg.Count(t, db, `SELECT count(*) FROM usage_billing_dedup WHERE request_id='retention-archive-replay'`),
		"replay's temporary hot claim must roll back when archive matches")
}
