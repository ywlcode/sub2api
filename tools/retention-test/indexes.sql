-- Within the same database, cleanup and VACUUM must preserve every definition.
SELECT tablename,indexname,indexdef,i.indisvalid,i.indisready
FROM pg_indexes p JOIN pg_class c ON c.relname=p.indexname
JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname=p.schemaname
JOIN pg_index i ON i.indexrelid=c.oid
WHERE p.schemaname='public' ORDER BY tablename,indexname;
