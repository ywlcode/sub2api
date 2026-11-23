-- Rows from 388 without acceptance are still outstanding claims. Never make
-- those eligible for automatic retry, even if their worker has disappeared.
ALTER TABLE monthly_usage_email_deliveries
    ADD COLUMN IF NOT EXISTS rejected_at timestamptz,
    ADD COLUMN IF NOT EXISTS uncertain_at timestamptz;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT FROM pg_constraint
        WHERE conrelid = 'monthly_usage_email_deliveries'::regclass
          AND conname = 'monthly_usage_one_delivery_outcome'
    ) THEN
        ALTER TABLE monthly_usage_email_deliveries ADD CONSTRAINT monthly_usage_one_delivery_outcome
            CHECK (num_nonnulls(accepted_at, rejected_at, uncertain_at) <= 1);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS monthly_usage_rejected_retry
    ON monthly_usage_email_deliveries (rejected_at, report_month)
    WHERE rejected_at IS NOT NULL;

-- The dispatch cursor covers all attempted recipients, including failures.
-- Honor months already completed by the previous scheduler on upgrade.
INSERT INTO settings (key, value)
SELECT 'ops_monthly_usage_last_dispatched_period', value FROM settings
WHERE key = 'ops_monthly_usage_last_completed_period'
ON CONFLICT (key) DO NOTHING;
