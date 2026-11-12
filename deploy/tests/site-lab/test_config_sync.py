"""Configuration propagation and wire size measurements; disposable mock lab only."""
import collections
import json
import sys
import time
from lab import Lab

lab = Lab(sys.argv[1])
assert lab.sql('primary', "SELECT name FROM accounts WHERE id=101") == 'domestic'
results=json.loads((lab.root/'evidence/config-sync.json').read_text()) if (lab.root/'evidence/config-sync.json').exists() else []

def record(name, rows):
    samples=[]
    for x in rows:
        sample={k:x.get(k) for k in ('at','full','rows','wire_bytes','raw_bytes','download_wire_bytes','download_json_bytes')}
        sample['table_counts']=dict(collections.Counter(x.get('tables',[])))
        samples.append(sample)
    if any(x["case"]==name for x in results): return
    results.append({'case':name,'samples':samples})
    (lab.root/'evidence/config-sync.json').write_text(json.dumps(results,indent=2))
    print(json.dumps(results[-1]),flush=True)

def since(at, **match):
    return [x for x in lab.records() if x['at']>=at and x['kind']=='sync' and x.get('status')==200 and all(x.get(k)==v for k,v in match.items())]

def changed(name, sql, ready):
    if any(x["case"]==name for x in results): return
    at=time.time();lab.sql('primary',sql)
    lab.eventually(lambda: ready() and lab.ready(),timeout=30,label=name)
    lab.eventually(lambda: since(at),timeout=10,label='sync evidence')
    record(name,[x for x in since(at) if x.get('rows')])

lab.eventually(lab.ready)
lab.eventually(lab.settled)
record('initial_full', [x for x in lab.records() if x.get('full') and x.get('status')==200][-1:])
at=time.time();lab.eventually(lambda:len(since(at,full=False,rows=0))>=3,timeout=22,label='idle heartbeats')
record('idle_heartbeat',since(at,full=False,rows=0))
changed('one_user_concurrency', 'UPDATE users SET concurrency=7 WHERE id=101',lambda:lab.sql('secondary','SELECT concurrency FROM users WHERE id=101')=='7')
changed('one_protocol_setting', "INSERT INTO settings(key,value) VALUES('enable_metadata_passthrough','true') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value",lambda:lab.sql('secondary',"SELECT value FROM settings WHERE key='enable_metadata_passthrough'")=='true')
changed('delete_setting', "DELETE FROM settings WHERE key='enable_metadata_passthrough'",lambda:lab.sql('secondary',"SELECT count(*) FROM settings WHERE key='enable_metadata_passthrough'")=='0')
changed('account_model_alias', "UPDATE accounts SET credentials=jsonb_set(credentials,'{model_mapping,deepseek-alias}','\"deepseek-chat\"') WHERE id=101",lambda:lab.sql('secondary',"SELECT credentials->'model_mapping'->>'deepseek-alias' FROM accounts WHERE id=101")=='deepseek-chat')
for protocol in ('responses','chat/completions','messages'):
    reply=lab.request(protocol=protocol,model='deepseek-alias',stream=True)
    assert reply['status']==200 and 'lab answer' in reply['body'] and 'response.failed' not in reply['body'],(protocol,reply)
lab.eventually(lab.settled)
# Primary's effective defaults must take precedence over deliberately different local boot settings.
p=lab.root/'config/primary.json';config=json.loads(p.read_text());config['gateway']['max_body_size']=262144;p.write_text(json.dumps(config,indent=2))
at=time.time();lab.command('restart','primary')
lab.eventually(lambda:lab.sql('secondary',"SELECT value->'gateway'->>'max_body_size' FROM site_replica_rows WHERE table_name='$policy'")=='262144' and lab.ready(),timeout=40,label='live primary body limit')
assert lab.request(text='x'*300000)['status']==413
record('effective_gateway_defaults_reload',since(at,full=True))
assert lab.sql('secondary',"SELECT value->'gateway'->>'text_max_body_size' FROM site_replica_rows WHERE table_name='$policy'")=='262144'
changed('account_pause',"INSERT INTO account_performance_monitors(account_id,paused) VALUES(101,true) ON CONFLICT(account_id) DO UPDATE SET paused=true",lambda:lab.sql('secondary','SELECT schedulable FROM accounts WHERE id=101')=='f')
assert lab.sql('primary','SELECT schedulable FROM accounts WHERE id=101')=='t'
changed('account_resume',"UPDATE account_performance_monitors SET paused=false WHERE account_id=101",lambda:lab.sql('secondary','SELECT schedulable FROM accounts WHERE id=101')=='t')
# Safety deliberately stays on the primary, per user scope.
at=time.time();lab.sql('primary',"""UPDATE settings SET value='true' WHERE key='risk_control_enabled';
INSERT INTO security_penalty_subjects(user_id,scope,subject_key,policy,blocked_until) VALUES(101,'session','primary-only','session_block',NOW()+INTERVAL '1 hour') ON CONFLICT DO NOTHING;
INSERT INTO settings(key,value) VALUES('smtp_password','primary-only-secret') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value;""")
lab.eventually(lambda:len(since(at))>=2,timeout=15)
assert lab.sql('secondary',"SELECT count(*) FROM site_replica_rows WHERE table_name IN ('security_penalty_subjects','content_moderation_user_state') OR (table_name='settings' AND row_key IN ('risk_control_enabled','security_penalty_config','smtp_password'))")=='0'
assert lab.request(stream=True)['status']==200
lab.sql('primary',"UPDATE settings SET value='false' WHERE key='risk_control_enabled'")
lab.eventually(lab.settled)
at=time.time()
import concurrent.futures
with concurrent.futures.ThreadPoolExecutor(max_workers=5) as pool:
    replies=list(pool.map(lambda _:lab.request(stream=True),range(5)))
assert all(x['status']==200 and 'response.completed' in x['body'] for x in replies)
lab.eventually(lab.settled,timeout=30)
lab.eventually(lambda:any(x.get('events') for x in since(at)),timeout=15)
record('five_requests_billing_and_traces', [x for x in since(at) if x.get('events')])
at=time.time();reply=lab.request(text='LAB_HTTP_ERROR');assert reply['status']>=400
lab.eventually(lab.settled,timeout=30);lab.eventually(lambda:any(x.get('events') for x in since(at)),timeout=15)
record('one_error_request', [x for x in since(at) if x.get('events')])
changed('large_fixture_added',"""INSERT INTO users(id,email,password_hash,balance,concurrency)
SELECT n,'scale-'||n||'@site-lab.invalid','disabled',100,5 FROM generate_series(1000,1999) n ON CONFLICT(id) DO NOTHING;
INSERT INTO api_keys(id,user_id,key,name,group_id) SELECT n,n,'sk-fixture-'||md5(n::text),'scale',101 FROM generate_series(1000,1999) n ON CONFLICT(id) DO NOTHING;""",lambda:lab.sql('secondary','SELECT count(*) FROM api_keys WHERE id>=1000')=='1000')
at=time.time();lab.command('restart','secondary');lab.eventually(lab.ready,timeout=35)
lab.eventually(lambda:since(at,full=True),timeout=15);record('full_with_1000_extra_users_keys',since(at,full=True))
changed('one_concurrency_after_scale','UPDATE users SET concurrency=8 WHERE id=101',lambda:lab.sql('secondary','SELECT concurrency FROM users WHERE id=101')=='8')
at=time.time();lab.eventually(lambda:len(since(at,full=False,rows=0))>=2,timeout=18);record('idle_after_scale',since(at,full=False,rows=0))
print(json.dumps({'passed':True,'counts':lab.query('primary',"SELECT (SELECT count(*) FROM users) users,(SELECT count(*) FROM api_keys) keys,(SELECT count(*) FROM accounts WHERE extra->>'site_secondary_enabled'='true') granted_accounts"),'evidence':str(lab.root/'evidence/config-sync.json')}),flush=True)
