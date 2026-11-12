"""Save final test evidence without exporting API keys or account credentials."""
import datetime
import hashlib
import json
import subprocess
import sys
from decimal import Decimal

from lab import Lab

lab = Lab(sys.argv[1])
dest = lab.root / "evidence"
phases = {}
for name in ("protocols", "policy", "delivery", "network", "lifecycle"):
    phases[name] = json.loads((dest / (name + ".json")).read_text())
    assert phases[name][-1]["test"] == "phase_passed", name
lab.eventually(lab.ready, label="healthy final replica")
lab.eventually(lab.settled, timeout=60, label="final outbox")
snapshots = {}
for node in ("primary", "secondary"):
    queries = {
        "identity": "SELECT * FROM site_identity",
        "users": "SELECT id,status,balance::text FROM users ORDER BY id",
        "api_key_quotas": "SELECT id,status,quota::text,quota_used::text FROM api_keys ORDER BY id",
        "subscriptions": "SELECT id,user_id,group_id,status,daily_usage_usd::text,weekly_usage_usd::text,monthly_usage_usd::text,daily_window_start,weekly_window_start,monthly_window_start FROM user_subscriptions ORDER BY id",
        "usage": "SELECT id,request_id,user_id,api_key_id,account_id,model,actual_cost::text,subscription_id,input_tokens,output_tokens,created_at FROM usage_logs ORDER BY id",
        "errors": "SELECT id,request_id,model,status_code,upstream_status_code,error_phase,error_type,error_message,resolved,created_at FROM ops_error_logs ORDER BY id",
        "traces": "SELECT trace_id,request_id,source,detail FROM request_connection_traces ORDER BY started_at",
        "outbox": "SELECT kind,count(*) FROM site_outbox GROUP BY kind",
        "receipts": "SELECT node_id,count(*) FROM site_receipts GROUP BY node_id",
        "billing_states": "SELECT state,count(*) FROM usage_billing_events GROUP BY state",
    }
    snapshots[node] = {name: lab.query(node, query) for name, query in queries.items()}
    (dest / (node + "-final.json")).write_text(json.dumps(snapshots[node], indent=2))
    (dest / (node + ".log")).write_text(lab.command("logs", "--no-color", node).stdout)
for node in ("link", "upstream"):
    (dest / (node + ".log")).write_text(lab.command("logs", "--no-color", node).stdout)
network = lab.command("exec", "-T", "link", "tc", "-s", "qdisc", "show", "dev", "eth0").stdout
assert "netem" not in network, network
(dest / "network-final.txt").write_text(network)
(dest / "containers.txt").write_text(lab.command("ps").stdout)
recorded = lab.records()
(dest / "link-records.json").write_text(json.dumps(recorded, indent=2))
accounting = {}
for name, data in snapshots.items():
    balance = next(r["balance"] for r in data["users"] if r["id"] == 101)
    accounting[name] = {"balance": balance, "usage_rows": len(data["usage"]), "outbox": data["outbox"]}
primary = snapshots["primary"]
cost = sum(Decimal(r["actual_cost"]) for r in primary["usage"] if r["user_id"] == 101 and r["subscription_id"] is None)
assert Decimal(accounting["primary"]["balance"]) == Decimal("100") - cost
assert accounting["primary"]["balance"] == accounting["secondary"]["balance"]
assert len([r for r in primary["usage"] if r["account_id"] == 101 and r["request_id"].startswith("site:")]) == len(snapshots["secondary"]["usage"])
sample = next(r for r in phases["network"] if r["test"] == "real_random_packet_loss")
loss_counts = {str(status): len([r for r in sample["samples"] if r["status"] == status]) for status in (200, 503)}
binary = lab.root / "build/sub2api"
with binary.open("rb") as handle:
    digest = hashlib.file_digest(handle, "sha256").hexdigest()
summary = {"collected_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "binary_sha256": digest,
           "urls": {node: lab.url(node) for node in ("primary", "secondary")}, "accounting": accounting,
           "random_loss_probe_status_counts": loss_counts, "phases": {name: len(rows)-1 for name, rows in phases.items()},
           "netem_removed": True, "secondary_ready": True}
(lab.root / "summary.json").write_text(json.dumps(summary, indent=2))
print(json.dumps(summary, indent=2))
