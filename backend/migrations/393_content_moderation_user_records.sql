-- Old auto_banned describes the user's status after a hit, not whether the hit
-- issued a new ban. Leave old decisions unknown instead of counting them twice.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS ban_applied BOOLEAN,
    ADD COLUMN IF NOT EXISTS ban_expires_at TIMESTAMPTZ;
ALTER TABLE content_moderation_user_state
    ADD COLUMN IF NOT EXISTS recorded_ban_count BIGINT NOT NULL DEFAULT 0;
