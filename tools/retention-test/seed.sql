\set ON_ERROR_STOP on
-- Synthetic data only: 60 days x 1,000 rows/day in each high-volume table.
-- Half-day offsets avoid races with wall-clock retention cutoffs.
CREATE TEMP TABLE retention_seed AS
SELECT i, md5('retention-'||i)::uuid AS trace_id,
 now() - ((i-1)/1000 + 0.5) * interval '1 day' AS at,
 (SELECT string_agg(md5(i::text||'-'||j),'') FROM generate_series(1,32) j) AS payload
FROM generate_series(1,60000) i;

INSERT INTO users(id,email,password_hash,balance) VALUES(910001,'retention@example.invalid','unusable-test-hash',100);
INSERT INTO accounts(id,name,platform,type) VALUES(910001,'retention synthetic','openai','apikey');
INSERT INTO api_keys(id,user_id,key,name) VALUES(910001,910001,'retention-test-not-a-real-key','retention synthetic');
INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,created_at)
VALUES(910001,910001,910001,'retention-usage-control','synthetic',now()-interval '20 days');

INSERT INTO request_connection_traces(trace_id,started_at,request_id,client_request_id,user_id,api_key_id,path,stream,detail)
SELECT trace_id,at,'request-'||i,'client-'||i,910001,910001,'/synthetic',true,jsonb_build_object('synthetic',payload) FROM retention_seed;
INSERT INTO request_timing_details(usage_log_id,trace_id,created_at,detail)
SELECT i,trace_id,at,jsonb_build_object('synthetic',payload) FROM retention_seed;
INSERT INTO usage_billing_events(request_id,api_key_id,request_fingerprint,schema_version,payload,state,observed_at,completed_at)
SELECT 'request-'||i,910001,md5(i::text),1,jsonb_build_object('synthetic',payload),
 CASE WHEN i%2=0 THEN 'complete' ELSE 'legacy' END,at,at FROM retention_seed;
INSERT INTO usage_billing_events(request_id,api_key_id,request_fingerprint,schema_version,payload,state,observed_at,completed_at)
SELECT 'protected-'||state,910001,'fp',1,'{}',state,now()-interval '59 days',NULL
FROM unnest(ARRAY['pending','applied','blocked']) state;
INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint,created_at)
SELECT 'request-'||i,910001,md5(i::text),at FROM retention_seed;
INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint,created_at) VALUES
 ('retention-hot-replay',910001,'retention-fixture-fingerprint',now()-interval '1 day'),
 ('retention-archive-replay',910001,'retention-fixture-fingerprint',now()-interval '20 days');
INSERT INTO user_traffic_events(event_id,user_id,api_key_id,request_id,endpoint,method,started_at,finished_at,upload_bytes,download_bytes,status_code,outcome,request_complete)
SELECT trace_id,910001,910001,'request-'||i,'/synthetic','POST',at,at+interval '1 second',1024,64,200,'success',true FROM retention_seed;
INSERT INTO user_traffic_hourly(bucket_start,user_id,api_key_id,upload_bytes,download_bytes,requests)
VALUES(date_trunc('hour',now()-interval '59 days'),910001,910001,61440000,3840000,60000);
ANALYZE;
