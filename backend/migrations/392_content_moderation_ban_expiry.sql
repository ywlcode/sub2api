-- NULL preserves the original release policy for bans issued before this migration.
ALTER TABLE content_moderation_user_state
    ADD COLUMN IF NOT EXISTS account_ban_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS openai_ban_expires_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_content_moderation_next_expiry
    ON content_moderation_user_state (LEAST(account_ban_expires_at, openai_ban_expires_at), user_id)
    WHERE LEAST(account_ban_expires_at, openai_ban_expires_at) IS NOT NULL;

-- Clear the old deadline on explicit release, even if a previous binary handles
-- that write. A later restriction must never inherit a former owner's deadline.
CREATE OR REPLACE FUNCTION clear_released_moderation_deadlines() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owned_status_revision IS NULL OR
       (NEW.owned_status_revision IS DISTINCT FROM OLD.owned_status_revision AND
        NEW.account_ban_expires_at IS NOT DISTINCT FROM OLD.account_ban_expires_at) THEN
        NEW.account_ban_expires_at := NULL;
    END IF;
    IF NOT NEW.openai_blocked THEN
        NEW.openai_ban_expires_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS clear_released_moderation_deadlines ON content_moderation_user_state;
CREATE TRIGGER clear_released_moderation_deadlines BEFORE UPDATE
    ON content_moderation_user_state FOR EACH ROW EXECUTE FUNCTION clear_released_moderation_deadlines();
