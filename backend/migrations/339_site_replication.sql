-- Sites use independent databases. Empty identity preserves standalone behavior.
CREATE TABLE site_identity (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 mode text NOT NULL CHECK (mode IN ('primary','secondary')),
 node_id text NOT NULL,
 primary_id text,
 version text NOT NULL DEFAULT ''
);
CREATE TABLE site_replica_rows (
 table_name text NOT NULL,
 row_key text NOT NULL,
 value jsonb NOT NULL,
 PRIMARY KEY(table_name,row_key)
);
CREATE TABLE site_outbox (
 id bigserial PRIMARY KEY,
 kind text NOT NULL CHECK (kind IN ('billing','ops_error_logs','request_connection_traces')),
 payload jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE site_receipts (
 node_id text NOT NULL,
 event_id bigint NOT NULL,
 digest text NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(node_id,event_id)
);
CREATE TABLE site_imported_logs (
 node_id text NOT NULL,
 table_name text NOT NULL,
 source_id text NOT NULL,
 local_id text NOT NULL,
 PRIMARY KEY(node_id,table_name,source_id)
);
CREATE TABLE site_account_grants (
 node_id text NOT NULL,
 account_id bigint NOT NULL,
 PRIMARY KEY(node_id,account_id)
);

-- Capture in the SAME transaction as the local canonical bill/log. The trigger
-- is inert on existing standalone deployments and primary sites.
CREATE FUNCTION site_capture_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM site_identity WHERE singleton AND mode='secondary') THEN RETURN NEW; END IF;
 IF TG_TABLE_NAME='usage_billing_events' THEN
   IF NEW.state IN ('applied','complete') AND OLD.state='pending' THEN
     INSERT INTO site_outbox(kind,payload) VALUES('billing',NEW.payload);
   END IF;
 ELSE
   INSERT INTO site_outbox(kind,payload) VALUES(TG_TABLE_NAME,to_jsonb(NEW));
 END IF;
 RETURN NEW;
END $$;
-- Capture triggers are installed only when binding a fresh secondary database.
-- Existing standalone/primary request paths incur no extra trigger lookups.
