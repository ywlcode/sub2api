"""Exercise real Sub2API containers and independent PostgreSQL/Redis instances."""
import argparse
import concurrent.futures
import hashlib
import json
import statistics
import time
import urllib.error
from decimal import Decimal

from lab import Lab

parser = argparse.ArgumentParser()
parser.add_argument("directory")
parser.add_argument("phase", choices=["protocols", "policy", "delivery", "network", "lifecycle"])
parser.add_argument("--lease-seconds", type=int, default=20 * 60,
                    help="Expected replica lease; use 60 when testing older binaries")
args = parser.parse_args()
if args.lease_seconds <= 0:
    parser.error("--lease-seconds must be positive")
lab = Lab(args.directory)
evidence = []
opening_accounting_basis = None


def record(name, **data):
    value = {"at": time.time(), "test": name, **data}
    evidence.append(value)
    with (lab.root / "evidence/all-results.jsonl").open("a") as archive:
        archive.write(json.dumps(value) + "\n")
    (lab.root / "evidence" / (args.phase + ".json")).write_text(json.dumps(evidence, indent=2))
    print(json.dumps(value), flush=True)


def success(r, stream=False, protocol="responses"):
    assert r["status"] == 200, r
    assert "lab answer" in r["body"], r
    if stream:
        terminal = {"responses": "response.completed", "messages": "message_stop", "chat/completions": "[DONE]"}[protocol]
        assert terminal in r["body"], r


def wait_sync():
    lab.eventually(lab.ready, timeout=45, label="replica ready")
    lab.eventually(lab.settled, timeout=65, label="outbox drained")


def accounting():
    wait_sync()
    primary = lab.query("primary", "SELECT balance::text, (SELECT coalesce(sum(actual_cost),0)::text FROM usage_logs WHERE user_id=101 AND subscription_id IS NULL) AS usage FROM users WHERE id=101")[0]
    assert Decimal(primary["balance"]) == opening_accounting_basis - Decimal(primary["usage"]), primary
    # Delivery ACK and configuration sync use independent connections. A debit
    # on a forwarded request can still await the next balance projection.
    target = Decimal(primary["balance"])
    lab.eventually(lambda: Decimal(lab.sql("secondary", "SELECT balance FROM users WHERE id=101")) == target,
                   timeout=15, label="authoritative balance installed on replica")
    secondary = lab.sql("secondary", "SELECT balance::text FROM users WHERE id=101;")
    assert Decimal(secondary) == target
    p = lab.sql("primary", "SELECT count(*) FROM usage_logs WHERE account_id=101 AND request_id LIKE 'site:%';")
    s = lab.sql("secondary", "SELECT count(*) FROM usage_logs;")
    assert p == s, (p, s)
    return {"primary_balance": primary["balance"], "secondary_balance": secondary, "primary_domestic_usage_rows": int(p), "secondary_usage_rows": int(s), "charged": primary["usage"]}


def expire(label):
    start = time.monotonic()
    lab.eventually(lambda: not lab.ready(), timeout=args.lease_seconds + 5,
                   label=f"{args.lease_seconds} second expiry")
    elapsed = time.monotonic() - start
    for model in ("deepseek-chat", "gpt-test"):
        r = lab.request(model=model)
        assert r["status"] == 503, r
    assert lab.http("secondary", "/health")["status"] == 200
    record(label, expected_lease_seconds=args.lease_seconds,
           stopped_after_fault_seconds=round(elapsed, 3), new_domestic_and_gpt_status=503, liveness=200)


def protocols():
    wait_sync()
    start_error_id = int(lab.sql("secondary", "SELECT coalesce(max(id),0) FROM ops_error_logs"))
    models_before = len([r for r in lab.records() if r.get("kind") == "model"])
    for protocol in ("responses", "chat/completions", "messages"):
        for stream in (False, True):
            r = lab.request(protocol=protocol, stream=stream)
            success(r, stream, protocol)
            record("domestic_protocol", protocol=protocol, stream=stream, status=r["status"], seconds=r["seconds"])
    for effort in ("high", "none"):
        r = lab.request(extra={"reasoning": {"effort": effort}})
        success(r)
        record("reasoning", effort=effort, status=200)
    history = [{"role": "user", "content": "continue from lab summary"},
               {"type": "reasoning", "summary": [{"type": "summary_text", "text": "lab summary"}]},
               {"type": "function_call", "id": "fc_lab", "call_id": "lab_call_1", "name": "read_file", "arguments": "{}"},
               {"type": "function_call_output", "call_id": "lab_call_1", "output": "file content"}]
    success(lab.request(extra={"input": history, "reasoning": {"effort": "high"}}))
    records = json.loads(lab.http("upstream", "/records")["body"])
    assert records[-1]["call_id_present"] and records[-1]["summary_present"], records[-1]
    assert len([r for r in lab.records() if r.get("kind") == "model"]) == models_before
    record("history_and_domestic_isolation", call_id=True, summary=True, model_requests_to_primary=0)
    before = int(lab.sql("secondary", "SELECT count(*) FROM usage_logs;"))
    for stream in (False, True):
        r = lab.request(model="gpt-test", text="gzip test " * 500, stream=stream)
        success(r, stream)
    forwarded = [r for r in lab.records() if r.get("kind") == "model"][-2:]
    expected = hashlib.sha256(("Bearer " + lab.creds["key"]).encode()).hexdigest()
    assert all(r["gzip"] and r["wire_bytes"] < r["raw_bytes"] and r["authorization_sha256"] == expected for r in forwarded), forwarded
    assert int(lab.sql("secondary", "SELECT count(*) FROM usage_logs;")) == before
    record("gpt_gzip_original_key_no_double_billing", forwarded=forwarded)
    keys = lab.query("secondary", "SELECT key FROM api_keys WHERE id=101")
    assert keys[0]["key"] != lab.creds["key"]
    assert lab.sql("secondary", "SELECT count(*) FROM accounts WHERE id=102;") == "0"
    assert lab.http("secondary", "/api/v1/admin/users")["status"] == 404
    record("replica_scope", raw_user_key_absent=True, foreign_credentials_absent=True, admin_status=404)
    r = lab.request(stream=True, text="LAB_STREAM_FAIL")
    assert ((r["status"] == 200 and "response.failed" in r["body"]) or r["status"] == 502) and "response.completed" not in r["body"], r
    record("http_200_upstream_stream_failure", upstream_status=200, response=r)
    r = lab.request(text="LAB_MISSING_USAGE", stream=True)
    assert r["status"] == 200, r
    lab.eventually(lambda: int(lab.sql("secondary", "SELECT count(*) FROM ops_error_logs WHERE error_type='site_usage_unsettled';")) > 0)
    record("missing_usage_durable_issue", response=r)
    r = lab.request(text="LAB_HTTP_ERROR")
    assert r["status"] >= 400, r
    record("upstream_error", response=r)
    wait_sync()
    # An empty outbox can precede the async ops writer. Match this phase's
    # individual diagnostics against the imported IDs, not two table sizes.
    local_query = ("SELECT id,request_id,error_type,error_message FROM ops_error_logs "
                   f"WHERE user_id=101 AND id>{start_error_id} ORDER BY id")
    required = {"site_usage_unsettled", "service_unavailable_error", "invalid_request_error"}
    def local_errors():
        rows = lab.query("secondary", local_query)
        return rows if required <= {r["error_type"] for r in rows} else None
    secondary_errors = lab.eventually(local_errors, timeout=30, label="local diagnostics persisted")
    ids = ",".join(f"'{int(r['id'])}'" for r in secondary_errors)
    expected = {int(r["id"]): (r["error_type"], r["error_message"]) for r in secondary_errors}
    def imported_errors():
        rows = lab.query("primary", "SELECT m.source_id,e.request_id,e.error_type,e.error_message "
                         "FROM site_imported_logs m JOIN ops_error_logs e ON e.id::text=m.local_id "
                         f"WHERE m.table_name='ops_error_logs' AND m.source_id IN ({ids}) AND e.user_id=101")
        actual = {int(r["source_id"]): (r["error_type"], r["error_message"]) for r in rows}
        return rows if actual == expected and len(rows) == len(expected) else None
    primary_errors = lab.eventually(imported_errors, timeout=30, label="every diagnostic imported once")
    record("diagnostics_and_accounting", primary_errors=primary_errors, secondary_errors=secondary_errors, accounting=accounting())


def policy():
    wait_sync()
    baseline = lab.sql("primary", "SELECT balance::text FROM users WHERE id=101;")
    cases = [
        ("revoked_key", "UPDATE api_keys SET status='disabled' WHERE id=101", "SELECT status='disabled' FROM api_keys WHERE id=101", "UPDATE api_keys SET status='active' WHERE id=101"),
        ("blocked_user", "UPDATE users SET status='disabled' WHERE id=101", "SELECT status='disabled' FROM users WHERE id=101", "UPDATE users SET status='active' WHERE id=101"),
        ("exhausted_balance", "UPDATE users SET balance=0 WHERE id=101", "SELECT balance=0 FROM users WHERE id=101", f"UPDATE users SET balance={baseline} WHERE id=101"),
        ("exhausted_key_quota", "UPDATE api_keys SET quota=0.00000001 WHERE id=101", "SELECT quota=0.00000001 FROM api_keys WHERE id=101", "UPDATE api_keys SET quota=100 WHERE id=101"),
        ("ip_restriction", "UPDATE api_keys SET ip_whitelist='[\"192.0.2.1\"]' WHERE id=101", "SELECT ip_whitelist IS NOT NULL FROM api_keys WHERE id=101", "UPDATE api_keys SET ip_whitelist=NULL WHERE id=101"),
        ("route_changed", "UPDATE composite_model_routes SET target_platform='openai',upstream_model='gpt-5.1' WHERE public_model='deepseek-chat'", "SELECT target_platform='openai' FROM composite_model_routes WHERE public_model='deepseek-chat'", "UPDATE composite_model_routes SET target_platform='deepseek',upstream_model='deepseek-chat' WHERE public_model='deepseek-chat'")]
    for name, change, visible, restore in cases:
        try:
            start = time.monotonic()
            lab.sql("primary", change + ";")
            lab.eventually(lambda: lab.sql("secondary", visible + ";") == "t", timeout=10, label=name)
            forwards_before = len([x for x in lab.records() if x.get("kind") == "model"])
            local_before = int(lab.sql("secondary", "SELECT count(*) FROM usage_logs;"))
            r = lab.request()
            if name == "route_changed":
                # A route moved to a foreign account is served by the primary.
                # It is a routing change, not an authorization denial.
                success(r)
                assert json.loads(r["body"])["model"] == "gpt-5.1", r
                assert len([x for x in lab.records() if x.get("kind") == "model"]) > forwards_before
                assert int(lab.sql("secondary", "SELECT count(*) FROM usage_logs;")) == local_before
            else:
                assert r["status"] >= 400, (name, r)
            record(name, sync_seconds=round(time.monotonic() - start, 3), status=r["status"], body=r["body"])
        finally:
            lab.sql("primary", restore + ";")
            time.sleep(5.3)
    success(lab.request())
    wait_sync()
    # A separately billed subscriber verifies local limits and authoritative usage.
    subkey = lab.creds["key"] + "s"
    lab.sql("primary", f"""
      INSERT INTO users(id,email,password_hash,balance,concurrency) VALUES(102,'subscriber@site-lab.invalid','disabled',0,10);
      INSERT INTO groups(id,name,platform,subscription_type,daily_limit_usd,weekly_limit_usd,monthly_limit_usd) VALUES(102,'lab-subscription','deepseek','subscription',1,2,3);
      INSERT INTO account_groups(account_id,group_id) VALUES(101,102);
      INSERT INTO api_keys(id,user_id,key,name,group_id) VALUES(102,102,'{subkey}','lab-subscriber',102);
      INSERT INTO user_subscriptions(id,user_id,group_id,starts_at,expires_at) VALUES(102,102,102,now(),now()+interval '30 days');
    """)
    lab.eventually(lambda: lab.sql("secondary", "SELECT count(*) FROM api_keys WHERE id=102;") == "1", timeout=10)
    success(lab.request(key=subkey))
    wait_sync()
    rows = lab.query("primary", "SELECT daily_usage_usd::text,weekly_usage_usd::text,monthly_usage_usd::text,daily_window_start,weekly_window_start,monthly_window_start FROM user_subscriptions WHERE id=102")
    assert Decimal(rows[0]["daily_usage_usd"]) > 0, rows
    assert lab.sql("primary", "SELECT balance FROM users WHERE id=102;") == "0.00000000"
    for period in ("daily", "weekly", "monthly"):
        lab.sql("primary", f"UPDATE groups SET {period}_limit_usd=0.00000001 WHERE id=102;")
        lab.eventually(lambda: lab.sql("secondary", f"SELECT {period}_limit_usd=0.00000001 FROM groups WHERE id=102;") == "t", timeout=10)
        r = lab.request(key=subkey)
        assert r["status"] >= 400, r
        record("subscription_" + period + "_limit", status=r["status"], body=r["body"])
        lab.sql("primary", f"UPDATE groups SET {period}_limit_usd=10 WHERE id=102;")
        time.sleep(5.3)
    record("subscription_windows_and_accounting", windows=rows, accounting=accounting())


def delivery():
    wait_sync()
    before = int(lab.sql("primary", "SELECT count(*) FROM usage_logs;"))
    lab.control(mode="lose_ack", remaining=1)
    success(lab.request())
    lab.eventually(lambda: any(r.get("lost_ack") for r in lab.records()), timeout=12, label="committed response discarded")
    lab.eventually(lab.settled, timeout=20)
    assert int(lab.sql("primary", "SELECT count(*) FROM usage_logs;")) == before + 1
    record("lost_ack_retransmit", lost_records=[r for r in lab.records() if r.get("lost_ack")], accounting=accounting())
    lab.control(mode="normal")
    wait_sync()
    lab.control(mode="reject")
    start = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        results = list(pool.map(lambda _: lab.request(), range(90)))
    for r in results:
        success(r)
    backlog = int(lab.sql("secondary", "SELECT count(*) FROM site_outbox;"))
    assert backlog > 128, backlog
    record("concurrent_durable_backlog", requests=len(results), seconds=round(time.monotonic()-start,3), outbox=backlog, mean_seconds=statistics.mean(r["seconds"] for r in results))
    expire("outage_backlog_expiry")
    # Kill the application after completion, retaining PostgreSQL and its outbox.
    lab.command("kill", "-s", "SIGKILL", "secondary")
    lab.command("start", "secondary")
    lab.eventually(lambda: lab.http("secondary", "/health")["status"] == 200, timeout=20)
    assert lab.request()["status"] == 503
    assert int(lab.sql("secondary", "SELECT count(*) FROM site_outbox;")) >= backlog
    record("restart_cannot_reuse_lease", domestic_status=503, outbox_preserved=True)
    recovery = time.monotonic()
    lab.control(mode="normal")
    wait_sync()
    record("paged_backlog_recovery", seconds=round(time.monotonic()-recovery,3), accounting=accounting())


def network():
    wait_sync()
    lab.control(mode="normal", delay=4)
    time.sleep(17)
    assert lab.ready()
    success(lab.request())
    record("four_second_control_delay", ready=True)
    lab.control(mode="normal", delay=18)
    expire("late_response_does_not_extend_lease")
    lab.control(mode="normal")
    wait_sync()
    # 60% independent packet loss + 140ms added delay on BOTH relay legs.
    lab.netem("delay", "140ms", "loss", "60%")
    samples = []
    started = time.monotonic()
    while time.monotonic()-started < 45:
        r = lab.request()
        assert r["status"] in (200, 503), r
        if r["status"] == 200:
            success(r)
        samples.append({"elapsed": round(time.monotonic()-started,3), "status": r["status"], "seconds": r["seconds"]})
        time.sleep(2)
    tc = lab.command("exec", "-T", "link", "tc", "-s", "qdisc", "show", "dev", "eth0").stdout
    record("real_random_packet_loss", loss_percent=60, added_egress_delay_ms=140, samples=samples, tc=tc)
    lab.clear_netem()
    wait_sync()
    # Start a streaming domestic request, then lose ALL primary connectivity.
    stream_seconds = args.lease_seconds + 15
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
        slow = pool.submit(lab.request, text=f"LAB_SLOW_EXPIRY:{stream_seconds}",
                           stream=True, timeout=stream_seconds + 30)
        time.sleep(1)
        lab.netem("loss", "100%")
        expire("complete_network_partition")
        assert not slow.done(), "the stream must still be in flight when the lease expires"
        result = slow.result(timeout=stream_seconds + 30)
        success(result, stream=True)
        assert result["seconds"] >= stream_seconds
        record("inflight_domestic_stream_survives_lease_expiry", seconds=result["seconds"], first_byte_seconds=result["first_byte_seconds"], completed=True)
    backlog = int(lab.sql("secondary", "SELECT count(*) FROM site_outbox;"))
    assert backlog > 0
    lab.clear_netem()
    recovery = time.monotonic()
    wait_sync()
    record("network_recovery", seconds=round(time.monotonic()-recovery,3), durable_backlog=backlog, accounting=accounting())


def lifecycle():
    for fault in ("wrong_token", "wrong_identity", "wrong_schema"):
        wait_sync()
        lab.control(mode=fault)
        expire(fault)
        lab.control(mode="normal")
        wait_sync()
    lab.command("stop", "primary")
    expire("primary_process_stopped")
    lab.command("start", "primary")
    wait_sync()
    success(lab.request())
    record("primary_restart_full_snapshot", latest_sync=[r for r in lab.records() if r.get("kind") == "sync"][-3:], accounting=accounting())
    conf = lab.root / "config/primary.json"
    original = conf.read_text()
    try:
        value = json.loads(original)
        value["site"] = {"mode": "standalone"}
        conf.write_text(json.dumps(value))
        lab.command("restart", "primary")
        lab.eventually(lambda: lab.http("primary", "/health")["status"] == 200, timeout=20)
        r = lab.http("primary", "/internal/sites/sync", {}, {"Authorization": "Bearer " + lab.creds["token"]})
        assert r["status"] == 404, r
        success(lab.request(site="primary"))
        expire("standalone_does_not_renew_secondary")
        record("standalone_gateway_operates_control_plane_disabled", model_status=200, sync_status=404)
    finally:
        conf.write_text(original)
        lab.command("restart", "primary")
    wait_sync()
    record("final_recovery", accounting=accounting())


try:
    # Compare each phase's debits against its opening ledger. A preceding
    # busy-sync test deliberately changes the balance with 4,000 tiny writes.
    # Persist the basis so rerunning a phase never hides those adjustments.
    wait_sync()
    opening_accounting_basis = Decimal(lab.sql("primary", "SELECT balance+(SELECT coalesce(sum(actual_cost),0) FROM usage_logs WHERE user_id=101 AND subscription_id IS NULL) FROM users WHERE id=101"))
    record("opening_accounting_basis", balance_plus_billed_usage=str(opening_accounting_basis))
    globals()[args.phase]()
    record("phase_passed", phase=args.phase)
except BaseException as exc:
    record("phase_failed", phase=args.phase, error=str(exc))
    raise
finally:
    lab.clear_netem()
    lab.control(mode="normal")
