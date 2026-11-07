\set ON_ERROR_STOP on
-- Full row-content checksums as well as counts, including the retained money,
-- usage detail, and hourly traffic rollup. No external accounts or credentials.
SELECT format('SELECT %L AS table_name, count(*) AS rows, md5(coalesce(string_agg(row_hash, '''' ORDER BY row_hash),'''')) AS content_hash FROM (SELECT md5(t::text) AS row_hash FROM %I t) hashes', name,name)
FROM unnest(ARRAY['users','accounts','api_keys','usage_logs','user_traffic_hourly',
 'request_connection_traces','request_timing_details','usage_billing_events',
 'usage_billing_dedup','usage_billing_dedup_archive','user_traffic_events']) name ORDER BY name
\gexec
-- pg_dump/restore can canonicalize equivalent expressions in unrelated older
-- indexes (e.g. casts outside vs inside ARRAY). Compare every index's identity
-- and validity, and exact definitions for the retention tables under test.
SELECT tablename,indexname,
 CASE WHEN tablename IN ('request_connection_traces','request_timing_details',
 'usage_billing_events','usage_billing_dedup','usage_billing_dedup_archive','user_traffic_events')
 THEN indexdef ELSE '' END,
 i.indisunique,i.indisprimary,i.indnatts,i.indisvalid,i.indisready
FROM pg_indexes p JOIN pg_class c ON c.relname=p.indexname
JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname=p.schemaname
JOIN pg_index i ON i.indexrelid=c.oid
WHERE p.schemaname='public' ORDER BY tablename,indexname;
