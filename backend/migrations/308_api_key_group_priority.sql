-- group_id remains the first choice; extra groups retain their configured order.
ALTER TABLE api_key_groups ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 0;

-- Preserve the order previously returned to clients for existing bindings.
WITH ranked AS (
    SELECT api_key_id, group_id,
           ROW_NUMBER() OVER (PARTITION BY api_key_id ORDER BY group_id)::INTEGER AS priority
    FROM api_key_groups
    WHERE priority = 0
)
UPDATE api_key_groups g SET priority = ranked.priority
FROM ranked
WHERE g.api_key_id = ranked.api_key_id AND g.group_id = ranked.group_id;
