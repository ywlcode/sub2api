-- Existing secondaries keep the legacy event namespace, including outstanding
-- bills. A newly bound database receives a random replica_id in bindIdentity.
ALTER TABLE site_identity ADD COLUMN replica_id text NOT NULL DEFAULT '';
ALTER TABLE site_outbox ADD COLUMN delivered_at timestamptz;
UPDATE site_outbox SET delivered_at=now() WHERE delivered;
ALTER TABLE site_rejected_events ADD COLUMN confirmed_at timestamptz;
-- Preserve both payloads on a same-incarnation identity conflict. Never replace
-- the original rejection/receipt with the conflicting event.
CREATE TABLE site_event_conflicts (
 node_id text NOT NULL, event_id bigint NOT NULL, digest text NOT NULL,
 kind text NOT NULL, payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(node_id,event_id,digest)
);

CREATE OR REPLACE FUNCTION site_capture_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target text := TG_TABLE_NAME; identity text;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM site_export_nodes) THEN RETURN NULL; END IF;
 -- Ordinary hot-row changes need only their ID. Full account credentials/extra
 -- need not be converted to JSON merely to notice a balance/quota update.
 IF target IN ('users','user_subscriptions','user_platform_quotas') THEN
   identity := CASE WHEN TG_OP='DELETE' THEN OLD.id::text ELSE NEW.id::text END;
 ELSIF target='usage_platform_quota_totals' THEN
   identity := CASE WHEN TG_OP='DELETE' THEN OLD.quota_id::text ELSE NEW.quota_id::text END;
 ELSIF target='accounts' THEN
   identity := CASE WHEN TG_OP='DELETE' THEN OLD.id::text ELSE NEW.id::text END;
   IF TG_OP<>'UPDATE' OR NEW.extra->'site_secondary_enabled' IS DISTINCT FROM OLD.extra->'site_secondary_enabled'
      OR NEW.deleted_at IS DISTINCT FROM OLD.deleted_at OR NEW.type IS DISTINCT FROM OLD.type
      OR NEW.parent_account_id IS DISTINCT FROM OLD.parent_account_id THEN target := '$scope'; identity := '*'; END IF;
 ELSIF target='api_keys' THEN
   identity := CASE WHEN TG_OP='DELETE' THEN OLD.id::text ELSE NEW.id::text END;
   IF TG_OP<>'UPDATE' OR NEW.group_id IS DISTINCT FROM OLD.group_id OR NEW.user_id IS DISTINCT FROM OLD.user_id
      OR NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN target := '$scope'; identity := '*'; END IF;
 ELSIF target IN ('api_key_groups','account_groups') THEN
   target := '$scope'; identity := '*';
 ELSE identity := '*';
 END IF;
 INSERT INTO site_changes(node_id,table_name,row_key)
 SELECT node_id,target,identity FROM site_export_nodes
 ON CONFLICT(node_id,table_name,row_key) DO UPDATE SET generation=EXCLUDED.generation;
 RETURN NULL;
END $$;
