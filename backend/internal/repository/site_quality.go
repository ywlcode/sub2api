package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/lib/pq"
	"time"
)

// Only completed quality verdicts are projected, not probe jobs or node latency.
const siteQualityRoundsSQL = `  SELECT p.account_id, p.id AS plan_id, COALESCE(NULLIF(r.pelican_config->>'model_id',''),p.model_id) AS model, r.quality_round_id, MAX(r.created_at) AS observed_at,
   bool_or(r.quality_judgment->>'verdict'='incorrect' AND r.status='failed'
    AND r.error_message IN ('answer_mismatch','state_degraded')
    AND (COALESCE(r.pelican_config->>'question_kind','')<>'random_reasoning'
      OR r.pelican_config->'quality_batch'->>'decision'='peer_passed')) AS failed,
   bool_and(COALESCE(r.quality_judgment->>'verdict'='correct' AND r.status='success'
    AND (COALESCE(r.pelican_config->>'question_kind','')<>'random_reasoning'
      OR r.pelican_config->'quality_batch'->>'decision'='passed'),false)) AS passed
  FROM scheduled_test_plans p JOIN scheduled_test_results r ON r.plan_id=p.id
  WHERE p.account_id=ANY($1) AND p.enabled
   AND jsonb_typeof(p.pelican_config->'quality')='object' AND r.quality_round_id<>''
   AND r.created_at >= $2 AND r.created_at >= p.updated_at AND r.created_at <= NOW()
   AND r.quality_action NOT IN ('','stale_run','comparison_pending')
  GROUP BY p.account_id,p.id,COALESCE(NULLIF(r.pelican_config->>'model_id',''),p.model_id),r.quality_round_id
  HAVING bool_and((r.pelican_config->'parallel_count' IS NULL OR jsonb_typeof(r.pelican_config->'parallel_count')='number')
    AND COALESCE(r.pelican_config->>'parallel_count','1') ~ '^[1-8]$')
   AND COUNT(*) = MAX(CASE WHEN COALESCE(r.pelican_config->>'parallel_count','1') ~ '^[1-8]$'
    THEN COALESCE(r.pelican_config->>'parallel_count','1')::int END)`

func exportSiteQuality(ctx context.Context, tx *sql.Tx, accounts []int64) ([]site.Row, error) {
	rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(q) FROM (`+siteQualityRoundsSQL+`) q`, pq.Array(accounts), time.Now().Add(-168*time.Hour))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []site.Row
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var identity struct {
			Account int64  `json:"account_id"`
			Plan    int64  `json:"plan_id"`
			Model   string `json:"model"`
			Round   string `json:"quality_round_id"`
		}
		if err = json.Unmarshal(raw, &identity); err != nil {
			return nil, err
		}
		key, _ := json.Marshal(identity)
		sum := sha256.Sum256(key)
		out = append(out, site.Row{Table: "$quality", Key: hex.EncodeToString(sum[:]), Value: raw})
	}
	return out, rows.Err()
}
