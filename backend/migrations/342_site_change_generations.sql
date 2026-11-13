-- Generation is an exact row token, never a sequence high-water mark. A late
-- commit or delete/reinsert must remain pending after snapshot acknowledgement.
CREATE SEQUENCE site_change_generation;
-- A constant for preexisting rows avoids rewriting the queue during upgrade.
-- Every subsequent insert/update obtains a fresh nonzero token.
ALTER TABLE site_changes ADD COLUMN generation bigint NOT NULL DEFAULT 0;
ALTER TABLE site_changes ALTER COLUMN generation SET DEFAULT nextval('site_change_generation');
ALTER SEQUENCE site_change_generation OWNED BY site_changes.generation;
CREATE OR REPLACE FUNCTION site_capture_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE data jsonb; previous jsonb; target text := TG_TABLE_NAME; identity text;
BEGIN
 data := CASE WHEN TG_OP='DELETE' THEN to_jsonb(OLD) ELSE to_jsonb(NEW) END;
 previous := CASE WHEN TG_OP='UPDATE' THEN to_jsonb(OLD) ELSE '{}'::jsonb END;
 identity := COALESCE(data->>'id',data->>'quota_id','*');
 IF target IN ('api_key_groups','account_groups') OR
    (target='api_keys' AND (TG_OP<>'UPDATE' OR data->'group_id' IS DISTINCT FROM previous->'group_id' OR data->'user_id' IS DISTINCT FROM previous->'user_id' OR data->'deleted_at' IS DISTINCT FROM previous->'deleted_at')) OR
    (target='accounts' AND (TG_OP<>'UPDATE' OR data->'extra'->'site_secondary_enabled' IS DISTINCT FROM previous->'extra'->'site_secondary_enabled' OR data->'deleted_at' IS DISTINCT FROM previous->'deleted_at' OR data->'type' IS DISTINCT FROM previous->'type' OR data->'parent_account_id' IS DISTINCT FROM previous->'parent_account_id')) THEN
   target := '$scope'; identity := '*';
 ELSIF target NOT IN ('users','api_keys','accounts','user_subscriptions','user_platform_quotas','usage_platform_quota_totals') THEN
   identity := '*';
 END IF;
 INSERT INTO site_changes(node_id,table_name,row_key)
 SELECT node_id,target,identity FROM site_export_nodes
 ON CONFLICT(node_id,table_name,row_key) DO UPDATE SET generation=EXCLUDED.generation;
 RETURN NULL;
END $$;
