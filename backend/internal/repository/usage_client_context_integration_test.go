//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestUsageClientContextPersistence(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"single", "batch", "best-effort-single", "best-effort-batch", "durable", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			billing, cmd, log, client := durableBillingFixture(t)
			repo := newUsageLogRepositoryWithSQL(client, billing.db)
			if mode != "legacy" {
				log.ClientContext = &service.UsageClientContext{Client: "codex", SessionID: "root", ThreadID: "child", ParentThreadID: "main", AgentRole: "subagent", RequestKind: "compaction"}
			}
			prepared := prepareUsageLogInsert(&log)
			switch mode {
			case "single", "legacy":
				_, err := repo.createSingle(ctx, billing.db, &log)
				require.NoError(t, err)
			case "batch":
				key := usageLogBatchKey(log.RequestID, log.APIKeyID)
				_, _, _, err := repo.batchInsertUsageLogs(billing.db, []string{key}, map[string]usageLogInsertPrepared{key: prepared})
				require.NoError(t, err)
			case "best-effort-single":
				require.NoError(t, execUsageLogInsertNoResult(ctx, billing.db, prepared))
			case "best-effort-batch":
				query, args := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
				_, err := billing.db.ExecContext(ctx, query, args...)
				require.NoError(t, err)
			case "durable":
				require.NoError(t, billing.StageUsageBilling(ctx, &cmd, &log))
				for i := 0; i < 2; i++ {
					_, err := billing.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: cmd.APIKeyID})
					require.NoError(t, err)
				}
			}
			got, err := scanUsageLog(billing.db.QueryRowContext(ctx, "SELECT "+usageLogSelectColumns+" FROM usage_logs WHERE request_id=$1 AND api_key_id=$2", log.RequestID, log.APIKeyID))
			require.NoError(t, err)
			require.Equal(t, log.ClientContext, got.ClientContext)
			require.InDelta(t, log.ActualCost, got.ActualCost, 1e-8)
		})
	}
}

func TestUsageClientContextMigrationPreservesOldRows(t *testing.T) {
	tx := testTx(t)
	_, err := tx.Exec(`CREATE TEMP TABLE usage_logs (id bigint, actual_cost numeric) ON COMMIT DROP; INSERT INTO usage_logs VALUES (1, 1.25)`)
	require.NoError(t, err)
	sql, err := migrations.FS.ReadFile("383_usage_client_context.sql")
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = tx.Exec(string(sql))
		require.NoError(t, err)
	}
	var untouched bool
	require.NoError(t, tx.QueryRow(`SELECT client_context IS NULL AND actual_cost=1.25 FROM usage_logs WHERE id=1`).Scan(&untouched))
	require.True(t, untouched)
}
