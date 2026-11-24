ALTER TABLE content_moderation_user_state
    ADD COLUMN IF NOT EXISTS all_platforms_blocked BOOLEAN NOT NULL DEFAULT FALSE;

CREATE OR REPLACE FUNCTION clear_released_moderation_deadlines() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT NEW.all_platforms_blocked AND (NEW.owned_status_revision IS NULL OR
       (NEW.owned_status_revision IS DISTINCT FROM OLD.owned_status_revision AND
        NEW.account_ban_expires_at IS NOT DISTINCT FROM OLD.account_ban_expires_at)) THEN
        NEW.account_ban_expires_at := NULL;
    END IF;
    IF NOT NEW.openai_blocked THEN
        NEW.openai_ban_expires_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;

-- A later administrator status write, even a repeated disable, has a different
-- revision. Only restore accounts whose current disable belongs to moderation.
WITH restored AS (
    UPDATE users u SET status='active',updated_at=NOW()
    FROM content_moderation_user_state s
    WHERE u.id=s.user_id AND u.deleted_at IS NULL AND u.status='disabled'
      AND u.role<>'admin' AND u.moderation_status_revision=s.owned_status_revision
    RETURNING u.id
)
UPDATE content_moderation_user_state s
SET all_platforms_blocked=TRUE, owned_status_revision=NULL, version=version+1, updated_at=NOW()
FROM restored WHERE s.user_id=restored.id;
