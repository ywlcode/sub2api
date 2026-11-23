-- Jobs created by older binaries have no owner and remain replay-only.
ALTER TABLE monthly_usage_manual_jobs ADD COLUMN IF NOT EXISTS owner_token uuid;
