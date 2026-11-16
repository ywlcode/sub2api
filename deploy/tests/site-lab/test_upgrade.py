"""Mixed-version assertions; only use a dedicated synthetic rebase lab.

Run 'mixed' after replacing primary with the candidate, keeping a real old
secondary binary. Run 'upgraded' after replacing secondary with the candidate.
The intentional new key restriction stays enabled across that transition.
"""
import argparse
import json
import time
from lab import Lab

parser = argparse.ArgumentParser()
parser.add_argument("directory")
parser.add_argument("stage", choices=("mixed", "legacy-features", "upgraded"))
args = parser.parse_args()
lab = Lab(args.directory)
assert "rebase" in str(lab.root), "use the isolated rebase lab"
evidence = {"stage": args.stage, "at": time.time()}


def success(reply):
    assert reply["status"] == 200 and "lab answer" in reply["body"], reply


lab.eventually(lab.ready, timeout=90, label="mixed version renewal")
if args.stage == "mixed":
    assert lab.sql("secondary", "SELECT count(*) FROM information_schema.columns WHERE table_name='api_keys' AND column_name='context_token_limit'") == "0", "must use the real old database/binary"
    for protocol in ("responses", "chat/completions", "messages"):
        for stream in (False, True):
            reply = lab.request(protocol=protocol, stream=stream)
            success(reply)
            if stream:
                terminal = {"responses": "response.completed", "chat/completions": "[DONE]", "messages": "message_stop"}[protocol]
                assert terminal in reply["body"] and "response.failed" not in reply["body"], reply
    success(lab.request(model="gpt-test", text="forward via gzip " * 500))
    lab.eventually(lab.settled, timeout=60)
    row = lab.query("primary", "SELECT platform,customer_cost_details,cache_creation_30m_tokens,cache_creation_12h_tokens FROM usage_logs WHERE user_id=101 AND request_id LIKE 'site:%' ORDER BY id DESC LIMIT 1")[0]
    assert row == {"platform": None, "customer_cost_details": None, "cache_creation_30m_tokens": 0, "cache_creation_12h_tokens": 0}, row
    evidence["old_bill_optional_defaults"] = row
    before = lab.sql("secondary", "SELECT version FROM site_identity WHERE singleton")
    lab.sql("primary", "ALTER TABLE users ADD COLUMN IF NOT EXISTS lab_future_optional text DEFAULT 'not-on-wire'; UPDATE users SET lab_future_optional='changed' WHERE id=101;")
    time.sleep(6)
    success(lab.request())
    assert lab.ready()
    evidence["unreviewed_database_column_ignored"] = True
    lab.sql("primary", "UPDATE api_keys SET context_token_limit=8192 WHERE id=101")
    lab.eventually(lambda: lab.sql("secondary", "SELECT status FROM api_keys WHERE id=101") == "inactive", timeout=12, label="legacy key fails closed for unsupported ceiling")
    reply = lab.request()
    assert reply["status"] in (401, 403), reply
    success(lab.request(site="primary"))
    evidence["legacy_secondary_limited_key_status"] = reply["status"]
    evidence["primary_limited_key_status"] = 200
    evidence["legacy_version"] = lab.sql("secondary", "SELECT version FROM site_identity WHERE singleton")
elif args.stage == "legacy-features":
    assert lab.sql("secondary", "SELECT count(*) FROM information_schema.columns WHERE table_name='api_keys' AND column_name='context_token_limit'") == "0", "requires the real old secondary"
    original = lab.query("primary", "SELECT platform,credentials FROM accounts WHERE id=101")[0]
    def literal(value):
        return "'" + value.replace("'", "''") + "'"
    try:
        evidence["forwarded_cases"] = []
        for case, platform in (("qwen", "qwen"), ("counted_cooldown", "deepseek"), ("cache_12h", "openai")):
            credentials = dict(original["credentials"])
            if case == "counted_cooldown":
                credentials.update(temp_unschedulable_enabled=True, temp_unschedulable_rules=[{"error_code":502,"keywords":["busy"],"duration_minutes":5,"trigger_count":3}])
            if case == "cache_12h":
                lab.sql("primary", "INSERT INTO channels(id,name) VALUES(785,'upgrade-cache-price'); INSERT INTO channel_model_pricing(channel_id,cache_write_12h_price) VALUES(785,0.000024)")
            lab.sql("primary", "UPDATE accounts SET platform="+literal(platform)+",credentials="+literal(json.dumps(credentials))+"::jsonb WHERE id=101; UPDATE composite_model_routes SET target_platform="+literal(platform)+" WHERE group_id=101 AND public_model='deepseek-chat'")
            # Direct fixture SQL does not publish the admin service's scheduler
            # invalidations. Reload the primary before testing the new platform.
            lab.command("restart", "primary")
            lab.eventually(lambda: lab.http("primary", "/health")["status"] == 200, timeout=30)
            lab.eventually(lambda: lab.sql("secondary", "SELECT NOT schedulable AND extra->'site_secondary_enabled'='false'::jsonb FROM accounts WHERE id=101") == "t", timeout=15, label=case+" legacy account fallback")
            before = len([r for r in lab.records() if r.get("kind") == "model"])
            success(lab.request())
            assert len([r for r in lab.records() if r.get("kind") == "model"]) > before
            assert lab.sql("primary", "SELECT schedulable AND extra->'site_secondary_enabled'='true'::jsonb FROM accounts WHERE id=101") == "t"
            evidence["forwarded_cases"].append(case)
    finally:
        lab.sql("primary", "UPDATE accounts SET platform="+literal(original["platform"])+",credentials="+literal(json.dumps(original["credentials"]))+"::jsonb WHERE id=101; UPDATE composite_model_routes SET target_platform='deepseek' WHERE group_id=101 AND public_model='deepseek-chat'; DELETE FROM channels WHERE id=785 AND name='upgrade-cache-price'")
        lab.command("restart", "primary")
        lab.eventually(lambda: lab.sql("secondary", "SELECT schedulable AND extra->'site_secondary_enabled'='true'::jsonb FROM accounts WHERE id=101") == "t", timeout=15, label="restore legacy account")
else:
    mixed = json.loads((lab.root / "evidence/upgrade-mixed.json").read_text())
    lab.eventually(lambda: lab.sql("secondary", "SELECT context_token_limit::text||':'||status FROM api_keys WHERE id=101") == "8192:active", timeout=30, label="upgrade replenishes new permission and restores key")
    version = lab.sql("secondary", "SELECT version FROM site_identity WHERE singleton")
    assert version != mixed["legacy_version"], "new contract must replace old projection epoch"
    success(lab.request())
    limited = lab.request(text="context word " * 20000)
    assert limited["status"] in (400, 403), limited
    evidence["context_limit_status"] = limited["status"]
    evidence["context_limit_error"] = limited["body"]
    evidence["new_version"] = version
    lab.sql("primary", "UPDATE api_keys SET context_token_limit=0 WHERE id=101")
    lab.eventually(lambda: lab.sql("secondary", "SELECT context_token_limit FROM api_keys WHERE id=101") == "0", timeout=12)
    # Same input now passes: distinguish the new per-key ceiling from an
    # unrelated model or legacy long-context rejection.
    success(lab.request(text="context word " * 20000))
    success(lab.request())
    lab.eventually(lab.settled, timeout=60)
    row = lab.query("primary", "SELECT platform,request_id FROM usage_logs WHERE user_id=101 AND request_id LIKE 'site:%' ORDER BY id DESC LIMIT 1")[0]
    assert row["platform"] == "deepseek" and row["request_id"].startswith("site:"), row
    evidence["new_bill_platform_and_source"] = row

(lab.root / "evidence" / ("upgrade-" + args.stage + ".json")).write_text(json.dumps(evidence, indent=2))
print(json.dumps(evidence))
