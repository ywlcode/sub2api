-- Platform bans are independent of users.status. Existing account disables retain
-- their ownership and require an explicit administrator recovery action.
ALTER TABLE content_moderation_user_state
    ADD COLUMN IF NOT EXISTS openai_blocked BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS ban_scope VARCHAR(16) NOT NULL DEFAULT 'all'
    CHECK (ban_scope IN ('all', 'openai'));
