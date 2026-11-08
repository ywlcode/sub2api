-- Reuse the durable invalidation worker for the event-driven security cache.
-- Settings and the notification are committed together. No request-time query
-- is needed to poll a revision; failed deliveries are retried by the outbox.
CREATE OR REPLACE FUNCTION enqueue_security_penalty_config_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    relevant BOOLEAN;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.key IS NOT DISTINCT FROM OLD.key AND NEW.value IS NOT DISTINCT FROM OLD.value THEN
            RETURN NEW;
        END IF;
        relevant := NEW.key IN ('security_penalty_config', 'risk_control_enabled', 'cyber_policy_user_allowlist')
                 OR OLD.key IN ('security_penalty_config', 'risk_control_enabled', 'cyber_policy_user_allowlist');
    ELSIF TG_OP = 'DELETE' THEN
        relevant := OLD.key IN ('security_penalty_config', 'risk_control_enabled', 'cyber_policy_user_allowlist');
    ELSE
        relevant := NEW.key IN ('security_penalty_config', 'risk_control_enabled', 'cyber_policy_user_allowlist');
    END IF;

    IF relevant THEN
        -- SHA-256("sub2api:security-penalty-config:v1"), a reserved identifier.
        INSERT INTO auth_cache_invalidation_outbox (cache_key)
        VALUES ('06d981b93f14b6407a0488f3be12b23cdddc8a22385b497573639edf9726a4ef');
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_security_penalty_config_invalidation ON settings;
CREATE TRIGGER trg_security_penalty_config_invalidation
AFTER INSERT OR UPDATE OR DELETE ON settings
FOR EACH ROW EXECUTE FUNCTION enqueue_security_penalty_config_invalidation();
