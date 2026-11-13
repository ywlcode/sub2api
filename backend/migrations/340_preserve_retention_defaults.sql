-- Fill only fields absent from older saved settings. Explicit administrator
-- choices are preserved. A malformed legacy value must not block an upgrade.
DO $$
DECLARE saved text; policy jsonb;
BEGIN
 SELECT value INTO saved FROM settings WHERE key='ops_advanced_settings' FOR UPDATE;
 IF saved IS NULL THEN RETURN; END IF;
 BEGIN policy := saved::jsonb;
 EXCEPTION WHEN invalid_text_representation THEN
   RAISE WARNING 'Invalid ops_advanced_settings; preserving value for administrator repair';
   RETURN;
 END;
 IF jsonb_typeof(policy)<>'object' THEN RETURN; END IF;
 IF NOT (policy ? 'data_retention') THEN
   policy := policy || '{"data_retention":{}}'::jsonb;
 END IF;
 IF jsonb_typeof(policy->'data_retention')<>'object' THEN RETURN; END IF;
 UPDATE settings SET value=jsonb_set(policy,'{data_retention}',
   '{"error_log_retention_days":30,"request_connection_retention_days":30,
     "request_timing_retention_days":30,"billing_dedup_hot_retention_days":15,
     "billing_dedup_archive_retention_days":350}'::jsonb || (policy->'data_retention'))::text,
   updated_at=NOW()
 WHERE key='ops_advanced_settings';
END $$;
