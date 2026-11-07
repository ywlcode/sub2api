-- Keep daily retention cleanup bounded as high-volume history grows without
-- blocking request writes while an existing production database upgrades.
CREATE INDEX CONCURRENTLY IF NOT EXISTS usage_billing_events_completed_retention_idx
    ON usage_billing_events (completed_at)
    WHERE state IN ('complete', 'legacy');

-- The archive is append-only and time-correlated, so BRIN keeps the supporting
-- index small while still avoiding a full-table scan for the daily cutoff.
CREATE INDEX CONCURRENTLY IF NOT EXISTS usage_billing_dedup_archive_created_at_brin
    ON usage_billing_dedup_archive USING BRIN (created_at);
