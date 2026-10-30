-- Existing users retain their total-concurrency policy.
ALTER TABLE users ADD COLUMN IF NOT EXISTS platform_concurrency_limits JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE OR REPLACE FUNCTION enqueue_user_platform_concurrency_invalidation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys k WHERE k.user_id = NEW.id AND k.deleted_at IS NULL;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_user_platform_concurrency_invalidation ON users;
CREATE TRIGGER trg_user_platform_concurrency_invalidation
AFTER UPDATE OF platform_concurrency_limits ON users
FOR EACH ROW WHEN (OLD.platform_concurrency_limits IS DISTINCT FROM NEW.platform_concurrency_limits)
EXECUTE FUNCTION enqueue_user_platform_concurrency_invalidation();
