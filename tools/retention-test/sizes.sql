SELECT c.relname AS table_name,
 pg_table_size(c.oid) AS table_bytes, pg_indexes_size(c.oid) AS index_bytes,
 pg_total_relation_size(c.oid) AS total_bytes
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname='public' AND c.relname IN
 ('request_connection_traces','request_timing_details','usage_billing_events',
  'usage_billing_dedup','usage_billing_dedup_archive','user_traffic_events')
ORDER BY c.relname;
SELECT pg_database_size(current_database()) AS database_bytes;
