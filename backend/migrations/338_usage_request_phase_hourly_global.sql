-- Admin usage request-phase averages: durable UTC hour buckets. User/key
-- averages continue to use their existing request-timing query and cache.
CREATE TABLE IF NOT EXISTS usage_request_phase_hourly_global (
    bucket_start TIMESTAMPTZ PRIMARY KEY,
    upload_sum_ms NUMERIC NOT NULL DEFAULT 0,
    processing_sum_ms NUMERIC NOT NULL DEFAULT 0,
    sample_count BIGINT NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Coalesce timing retries, late traces and usage deletions by their original
-- usage hour. Workers lock a dirty row until its bucket has been recomputed.
CREATE TABLE IF NOT EXISTS usage_request_phase_hourly_dirty (
    bucket_start TIMESTAMPTZ PRIMARY KEY,
    marked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION queue_usage_request_phase_hour_dirty(usage_time TIMESTAMPTZ)
RETURNS VOID LANGUAGE plpgsql AS $$
DECLARE
    hour_start TIMESTAMPTZ;
BEGIN
    IF usage_time IS NULL THEN
        RETURN;
    END IF;
    hour_start := date_trunc('hour', usage_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    IF hour_start < (date_trunc('hour', clock_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') - INTERVAL '24 hours' THEN
        RETURN;
    END IF;
    -- The current hour has not been aggregated yet. Avoid a hot-row write on
    -- every ordinary request; the worker seeds that hour after it completes.
    IF hour_start >= date_trunc('hour', clock_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' THEN
        RETURN;
    END IF;
    INSERT INTO usage_request_phase_hourly_dirty (bucket_start, marked_at)
    VALUES (hour_start, clock_timestamp())
    ON CONFLICT (bucket_start) DO UPDATE SET marked_at = EXCLUDED.marked_at;
END;
$$;

CREATE OR REPLACE FUNCTION mark_usage_request_phase_hour_dirty()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    old_time TIMESTAMPTZ;
    new_time TIMESTAMPTZ;
BEGIN
    IF TG_ARGV[0] = 'usage' THEN
        IF TG_OP IN ('DELETE', 'UPDATE') THEN
            old_time := OLD.created_at;
        END IF;
        IF TG_OP IN ('INSERT', 'UPDATE') THEN
            new_time := NEW.created_at;
        END IF;
    ELSE
        IF TG_OP IN ('DELETE', 'UPDATE') THEN
            SELECT created_at INTO old_time FROM usage_logs WHERE id = OLD.usage_log_id;
        END IF;
        IF TG_OP IN ('INSERT', 'UPDATE') THEN
            SELECT created_at INTO new_time FROM usage_logs WHERE id = NEW.usage_log_id;
        END IF;
    END IF;
    IF old_time IS NOT NULL THEN
        PERFORM queue_usage_request_phase_hour_dirty(old_time);
    END IF;
    IF new_time IS NOT NULL AND new_time IS DISTINCT FROM old_time THEN
        PERFORM queue_usage_request_phase_hour_dirty(new_time);
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS usage_request_phase_detail_dirty ON request_timing_details;
CREATE TRIGGER usage_request_phase_detail_dirty
AFTER INSERT OR UPDATE OR DELETE ON request_timing_details
FOR EACH ROW EXECUTE FUNCTION mark_usage_request_phase_hour_dirty('detail');

DROP TRIGGER IF EXISTS usage_request_phase_usage_dirty ON usage_logs;
CREATE TRIGGER usage_request_phase_usage_dirty
AFTER INSERT OR DELETE OR UPDATE OF created_at ON usage_logs
FOR EACH ROW EXECUTE FUNCTION mark_usage_request_phase_hour_dirty('usage');
