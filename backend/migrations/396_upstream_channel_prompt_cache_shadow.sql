-- Preserve observation for channels present before this switch was introduced.
-- The backfill runs only when adding the column; replays must retain later
-- explicit opt-outs and the default false on newly created channels.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'upstream_channels'::regclass
          AND attname = 'prompt_cache_shadow_enabled' AND NOT attisdropped
    ) THEN
        ALTER TABLE upstream_channels
            ADD COLUMN prompt_cache_shadow_enabled BOOLEAN NOT NULL DEFAULT false;
        UPDATE upstream_channels
        SET prompt_cache_shadow_enabled = true,
            revision = revision + 1,
            updated_at = CURRENT_TIMESTAMP;
    END IF;
END
$$;

COMMENT ON TABLE upstream_channels IS 'Explicit current analytics ownership and prompt-cache shadow observation only; unrelated to model-pricing channels and does not affect billing or routing';
COMMENT ON COLUMN upstream_channels.prompt_cache_shadow_enabled IS 'Prompt-cache shadow observation only for explicitly bound, non-deleted OpenAI accounts; does not affect billing or routing. Existing channels start enabled; new channels default to false.';
