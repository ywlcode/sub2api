-- Nullable and without a stored expression: adding this column does not rewrite
-- the large JSON table. New traces write the scalar directly; a bounded worker
-- backfills recent history first, independently of application startup.
ALTER TABLE request_connection_traces ADD COLUMN IF NOT EXISTS body_bytes BIGINT;

CREATE TABLE IF NOT EXISTS connection_trace_body_bytes_backfill (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 before_time TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 after_trace_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
 oldest_time TIMESTAMPTZ NOT NULL DEFAULT NOW() - INTERVAL '30 days',
 completed BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO connection_trace_body_bytes_backfill (id) VALUES (1) ON CONFLICT DO NOTHING;
