-- Optional client-declared session / agent attribution. No backfill or default:
-- historical rows and events from older secondaries retain unknown attribution.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS client_context JSONB;
COMMENT ON COLUMN usage_logs.client_context IS 'Bounded client-declared session, thread and agent metadata; display only, never an authorization or billing identity';
