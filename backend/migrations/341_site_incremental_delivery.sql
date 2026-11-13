-- Independent delivery and policy cursors; never infer receipts from serial IDs.
ALTER TABLE site_outbox ADD COLUMN attempted boolean NOT NULL DEFAULT false;
ALTER TABLE site_outbox ADD COLUMN delivered boolean NOT NULL DEFAULT false;
-- v3 may have lost an acknowledgement after the primary committed. Existing
-- bills must participate in receipt lookup before publishing a v4 lease.
UPDATE site_outbox SET attempted=true WHERE kind='billing';
CREATE INDEX site_outbox_delivery ON site_outbox(kind,id) WHERE NOT delivered;
CREATE INDEX site_outbox_diagnostics ON site_outbox(id) WHERE kind<>'billing' AND NOT delivered;
CREATE INDEX site_outbox_billing_ack ON site_outbox(id) WHERE kind='billing' AND attempted;
ALTER TABLE site_receipts ADD COLUMN rejection text NOT NULL DEFAULT '';
CREATE TABLE site_rejected_events (
 node_id text NOT NULL, event_id bigint NOT NULL, kind text NOT NULL,
 payload jsonb NOT NULL, reason text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(node_id,event_id)
);
CREATE TABLE site_export_nodes (
 node_id text PRIMARY KEY, fingerprint text NOT NULL, epoch text NOT NULL,
 revision bigint NOT NULL DEFAULT 0, initialized boolean NOT NULL DEFAULT false
);
CREATE TABLE site_export_rows (
 node_id text NOT NULL REFERENCES site_export_nodes(node_id) ON DELETE CASCADE,
 table_name text NOT NULL, row_key text NOT NULL, value jsonb,
 revision bigint NOT NULL, PRIMARY KEY(node_id,table_name,row_key)
);
CREATE INDEX site_export_rows_revision ON site_export_rows(node_id,revision);
CREATE INDEX site_export_rows_cursor ON site_export_rows(node_id,((table_name||':'||row_key) COLLATE "C")) INCLUDE(revision);
CREATE TABLE site_changes (
 node_id text NOT NULL REFERENCES site_export_nodes(node_id) ON DELETE CASCADE,
 table_name text NOT NULL, row_key text NOT NULL,
 PRIMARY KEY(node_id,table_name,row_key)
);
-- Installed only on primaries. The dirty set is consumed in the same transaction
-- as projection updates. Concurrent writers enqueue after its row lock releases;
-- there is no sequence high-water that can skip a late commit.
CREATE FUNCTION site_capture_change() RETURNS trigger LANGUAGE plpgsql AS $$
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
 ON CONFLICT(node_id,table_name,row_key) DO UPDATE SET row_key=EXCLUDED.row_key;
 RETURN NULL;
END $$;
CREATE OR REPLACE FUNCTION site_capture_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM site_identity WHERE singleton AND mode='secondary') THEN RETURN NEW; END IF;
 IF TG_TABLE_NAME='usage_billing_events' THEN
   IF NEW.state IN ('applied','complete') AND OLD.state='pending' THEN
     INSERT INTO site_outbox(kind,payload) VALUES('billing',NEW.payload);
   END IF;
 ELSIF NEW.api_key_id IS NOT NULL AND NEW.api_key_id>0 THEN
   -- Anonymous failures stay in local diagnostics; they cannot amplify upload.
   INSERT INTO site_outbox(kind,payload) VALUES(TG_TABLE_NAME,to_jsonb(NEW));
 END IF;
 RETURN NEW;
END $$;
