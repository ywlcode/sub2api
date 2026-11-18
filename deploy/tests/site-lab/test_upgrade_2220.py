"""2.22.0 mixed-binary checks; use only a dedicated synthetic rebase lab.

Run mixed after upgrading primary, then upgraded after upgrading secondary.
The new service-tier option is set only on an unused fixture group during mixed mode.
"""
import argparse
import json
import time
from lab import Lab

parser = argparse.ArgumentParser()
parser.add_argument("directory")
parser.add_argument("stage", choices=("mixed", "upgraded"))
args = parser.parse_args()
lab = Lab(args.directory)
assert "rebase" in str(lab.root), "use the isolated rebase lab"
evidence = {"stage": args.stage, "at": time.time()}


def success(reply, terminal=None):
    assert reply["status"] == 200 and "lab answer" in reply["body"], reply
    if terminal:
        assert terminal in reply["body"] and "response.failed" not in reply["body"], reply


lab.eventually(lab.ready, timeout=90, label="replica ready")
if args.stage == "mixed":
    assert lab.sql("secondary", "SELECT count(*) FROM information_schema.columns WHERE table_name='groups' AND column_name='force_openai_ultrafast'") == "0", "must use the real old binary and database"
    conf = lab.root / "config/primary.json"
    config = json.loads(conf.read_text())
    saved_gateway = lab.root / "evidence/upgrade-2220-original-gateway.json"
    saved_gateway.write_text(json.dumps(config.get("gateway", {})))
    config.setdefault("gateway", {}).setdefault("openai_ws", {}).update(
        client_history_recovery_enabled=True, lossy_history_restart_enabled=True)
    conf.write_text(json.dumps(config))
    lab.command("restart", "primary")
    lab.eventually(lambda: lab.http("primary", "/health")["status"] == 200, timeout=60)
    # No account, key, or user uses this group: old receivers must ignore the
    # optional field, but must not run newly enabled billing semantics.
    lab.sql("primary", "INSERT INTO groups(id,name,platform,subscription_type,force_openai_ultrafast) VALUES(2220,'upgrade-unused-ultrafast','openai','standard',true)")
    lab.eventually(lambda: lab.sql("secondary", "SELECT name FROM groups WHERE id=2220") == "upgrade-unused-ultrafast", timeout=15)
    for protocol, terminal in (("responses", "response.completed"), ("chat/completions", "[DONE]"), ("messages", "message_stop")):
        for stream in (False, True):
            success(lab.request(protocol=protocol, stream=stream), terminal if stream else None)
    success(lab.request(model="gpt-test", stream=True, text="forward via gzip " * 500), "response.completed")
    lab.eventually(lab.settled, timeout=60)
    old = lab.query("primary", "SELECT platform,customer_cost_details,cache_creation_30m_tokens,cache_creation_12h_tokens FROM usage_logs WHERE user_id=101 AND request_id LIKE 'site:%' ORDER BY id DESC LIMIT 1")[0]
    assert old["platform"] == "deepseek" and old["customer_cost_details"] is None, old
    assert old["cache_creation_30m_tokens"] == old["cache_creation_12h_tokens"] == 0, old
    evidence["old_binary_bill_accepted"] = old
    evidence["old_secondary_ignores_optional_column"] = True
    evidence["old_secondary_accepts_unknown_optional_gateway_policy"] = True
    # Unknown physical DB columns must not silently become a wire requirement.
    lab.sql("primary", "ALTER TABLE users ADD COLUMN IF NOT EXISTS lab_future_optional text DEFAULT 'not-on-wire'; UPDATE users SET lab_future_optional='changed' WHERE id=101")
    time.sleep(6)
    success(lab.request())
    assert lab.ready()
    evidence["unreviewed_column_does_not_break_renewal"] = True
    lab.eventually(lab.settled, timeout=60)
    lab.command("stop", "secondary")
    try:
        success(lab.request(site="primary", stream=True), "response.completed")
        evidence["primary_works_without_secondary"] = True
    finally:
        lab.command("start", "secondary")
        lab.urls.clear()
        lab.eventually(lab.ready, timeout=60)
else:
    lab.eventually(lambda: lab.sql("secondary", "SELECT force_openai_ultrafast FROM groups WHERE id=2220") == "t", timeout=30, label="upgraded replica replenishes new field")
    evidence["new_optional_field_replenished"] = True
    # A subsequent incremental update must invalidate the installed copy too.
    lab.sql("primary", "UPDATE groups SET force_openai_ultrafast=false WHERE id=2220")
    lab.eventually(lambda: lab.sql("secondary", "SELECT force_openai_ultrafast FROM groups WHERE id=2220") == "f", timeout=15)
    evidence["new_field_incremental_update"] = True
    for protocol, terminal in (("responses", "response.completed"), ("chat/completions", "[DONE]"), ("messages", "message_stop")):
        success(lab.request(protocol=protocol, stream=True), terminal)
    success(lab.request(model="gpt-test", stream=True, text="forward via gzip " * 500), "response.completed")
    lab.eventually(lab.settled, timeout=60)
    lab.sql("primary", "DELETE FROM groups WHERE id=2220")
    # Replica deletion keeps a tombstone to preserve referenced history.
    lab.eventually(lambda: lab.sql("secondary", "SELECT count(*) FROM groups WHERE id=2220 AND deleted_at IS NULL") == "0", timeout=15)
    conf = lab.root / "config/primary.json"
    config = json.loads(conf.read_text())
    config["gateway"] = json.loads((lab.root / "evidence/upgrade-2220-original-gateway.json").read_text())
    conf.write_text(json.dumps(config))
    lab.command("restart", "primary")
    lab.eventually(lambda: lab.http("primary", "/health")["status"] == 200, timeout=60)
    lab.eventually(lab.ready, timeout=60)

(lab.root / "evidence" / ("upgrade-2220-" + args.stage + ".json")).write_text(json.dumps(evidence, indent=2))
print(json.dumps(evidence))
