-- Rejections retain their original evidence and an explicit reconciliation state.
ALTER TABLE site_rejected_events ADD COLUMN state text NOT NULL DEFAULT 'pending'
 CHECK (state IN ('pending','retrying','settled','waived'));
ALTER TABLE site_rejected_events ADD COLUMN resolution_note text NOT NULL DEFAULT '';
ALTER TABLE site_rejected_events ADD COLUMN resolved_at timestamptz;
ALTER TABLE site_rejected_events ADD COLUMN resolved_by bigint;
ALTER TABLE site_rejected_events ADD COLUMN attempts integer NOT NULL DEFAULT 1;
ALTER TABLE site_rejected_events ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX site_rejected_events_pending ON site_rejected_events(node_id,state,event_id);
ALTER TABLE site_outbox ADD COLUMN available_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX site_outbox_ready ON site_outbox(available_at,id) WHERE NOT delivered;

-- Queue only resolution metadata for the owning secondary, never raw payloads.
CREATE FUNCTION site_capture_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM site_identity WHERE singleton AND mode='primary')
 AND NEW.state IN ('settled','waived') THEN
  INSERT INTO site_changes(node_id,table_name,row_key)
  SELECT node_id,'$settlements',NEW.event_id::text FROM site_export_nodes WHERE node_id=NEW.node_id
  ON CONFLICT(node_id,table_name,row_key) DO UPDATE SET generation=EXCLUDED.generation;
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER site_settlement_change AFTER INSERT OR UPDATE ON site_rejected_events
 FOR EACH ROW EXECUTE FUNCTION site_capture_settlement();
