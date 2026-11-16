-- Unknown legacy receipts and billing receipts remain permanent. Only newly
-- classified diagnostics can expire, after a durable timestamp floor is set.
ALTER TABLE site_receipts ADD COLUMN diagnostic_kind text;
ALTER TABLE site_receipts ADD COLUMN diagnostic_at timestamptz;
CREATE INDEX site_receipts_diagnostic_retention ON site_receipts(diagnostic_kind,diagnostic_at)
 WHERE diagnostic_at IS NOT NULL AND rejection='';
CREATE TABLE site_diagnostic_retention (
 table_name text PRIMARY KEY CHECK(table_name IN ('ops_error_logs','request_connection_traces')),
 discarded_before timestamptz NOT NULL DEFAULT '-infinity'
);
INSERT INTO site_diagnostic_retention(table_name) VALUES('ops_error_logs'),('request_connection_traces');

-- Cursors older than reclaimed tombstones must receive a complete snapshot.
ALTER TABLE site_export_nodes ADD COLUMN retained_revision bigint NOT NULL DEFAULT 0;
CREATE INDEX site_export_rows_tombstones ON site_export_rows(node_id,revision) WHERE value IS NULL;

-- Local cleanup checks durable unsettled identities without scanning payloads.
CREATE INDEX site_outbox_billing_identity ON site_outbox
 ((payload#>>'{command,RequestID}'),(payload#>>'{command,APIKeyID}')) WHERE kind='billing';
CREATE INDEX site_rejected_billing_identity ON site_rejected_events
 ((payload#>>'{command,RequestID}'),(payload#>>'{command,APIKeyID}'))
 WHERE kind='billing' AND state NOT IN ('settled','waived');
