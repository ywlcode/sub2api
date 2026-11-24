-- The primary installs this trigger alongside the existing site change triggers.
-- Use the same generation fence as ordinary user and key updates. A release or
-- expiry can race an in-flight export and must remain visible on the next poll.
CREATE FUNCTION site_capture_moderation_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    uid bigint := COALESCE(NEW.user_id, OLD.user_id);
BEGIN
    IF COALESCE(NEW.all_platforms_blocked, false) <> COALESCE(OLD.all_platforms_blocked, false) THEN
        INSERT INTO site_changes(node_id, table_name, row_key)
        SELECT node_id, 'users', uid::text FROM site_export_nodes
        ON CONFLICT(node_id, table_name, row_key) DO UPDATE SET generation=EXCLUDED.generation;
    END IF;
    IF COALESCE(NEW.openai_blocked, false) <> COALESCE(OLD.openai_blocked, false) THEN
        INSERT INTO site_changes(node_id, table_name, row_key)
        SELECT n.node_id, 'api_keys', k.id::text FROM site_export_nodes n
        CROSS JOIN api_keys k WHERE k.user_id=uid AND k.deleted_at IS NULL
        ON CONFLICT(node_id, table_name, row_key) DO UPDATE SET generation=EXCLUDED.generation;
    END IF;
    RETURN NULL;
END $$;
