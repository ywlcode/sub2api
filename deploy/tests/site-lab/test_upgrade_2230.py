"""2.23.0 rolling upgrade. Use only with the isolated old/new binary lab."""
import json
import sys

from lab import Lab

lab = Lab(sys.argv[1])
stage = sys.argv[2]
assert stage in {"mixed", "upgraded"} and "rebase" in str(lab.root)
assert lab.sql("primary", "SELECT name FROM accounts WHERE id=101") == "domestic"
price = "SELECT ultrafast_multiplier FROM channel_model_pricing WHERE id=22300"
marker = lab.root / "evidence/upgrade-2230-mixed.json"
query = lab.sql
lab.eventually(lab.ready, timeout=90, label="secondary acquired lease")
report = {"stage": stage}


def exercise():
    for protocol, terminal in (("responses", "response.completed"), ("chat/completions", "[DONE]"), ("messages", "message_stop")):
        for stream in (False, True):
            r = lab.request(protocol=protocol, stream=stream)
            assert r["status"] == 200 and "lab answer" in r["body"], (protocol, stream, r)
            if stream:
                assert terminal in r["body"] and "response.failed" not in r["body"] and "response.incomplete" not in r["body"], r
    r = lab.request(model="gpt-test", stream=True, text="forward via gzip " * 500)
    assert r["status"] == 200 and "response.completed" in r["body"] and "lab answer" in r["body"], r
    lab.eventually(lab.settled, timeout=60)


if stage == "mixed":
    assert query("secondary", "SELECT count(*) FROM information_schema.columns WHERE table_name='channel_model_pricing' AND column_name='ultrafast_multiplier'") == "0"
    query("primary", "BEGIN; INSERT INTO channels(id,name) VALUES(22300,'unused-upgrade-2230'); INSERT INTO channel_model_pricing(id,channel_id,ultrafast_multiplier) VALUES (22300,22300,2.5); COMMIT")
    lab.eventually(lambda: query("secondary", "SELECT count(*) FROM channel_model_pricing WHERE id=22300") == "1", timeout=30)
    report["old_binary_ignored_new_price_field"] = True
    start = query("primary", "SELECT COALESCE(max(id),0) FROM usage_logs")
    exercise()
    # Match only charges from this run, posted by the old binary. The new
    # primary must use its applied debit, not fabricate it from a quote.
    result = lab.query("primary", f"SELECT count(*) AS n,count(DISTINCT u.request_id) AS distinct_ids,count(q.wallet_debit) AS wallet_proofs FROM usage_logs u LEFT JOIN usage_billing_quotes q ON q.request_id=u.request_id AND q.api_key_id=u.api_key_id AND q.debited_user_id=u.user_id AND q.wallet_debit=u.actual_cost WHERE u.user_id=101 AND u.request_id LIKE 'site:%' AND u.id>{int(start)}")
    row = result[0]
    assert row["n"] == row["distinct_ids"] == 6, row
    # Not every price path emits a quote. An unquoted bill must still settle.
    quoted = query("primary", f"SELECT count(*) FROM usage_logs u JOIN usage_billing_quotes q USING(request_id,api_key_id) WHERE u.user_id=101 AND u.request_id LIKE 'site:%' AND u.id>{int(start)}")
    assert int(quoted) == row["wallet_proofs"], row
    report["accepted_old_binary_bills"] = row
    lab.command("stop", "secondary")
    try:
        r = lab.request(site="primary", stream=True)
        assert r["status"] == 200 and "response.completed" in r["body"] and "lab answer" in r["body"], r
    finally:
        lab.command("start", "secondary")
        lab.urls.clear()
        lab.eventually(lab.ready, timeout=90)
    report["primary_served_while_secondary_down"] = True
else:
    assert marker.exists()
    lab.eventually(lambda: float(query("secondary", price) or 0) == 2.5, timeout=30, label="full refresh repopulates price")
    query("primary", "UPDATE channel_model_pricing SET ultrafast_multiplier=3.125 WHERE id=22300")
    lab.eventually(lambda: float(query("secondary", price) or 0) == 3.125, timeout=30, label="incremental price change")
    query("primary", "UPDATE channel_model_pricing SET ultrafast_multiplier=NULL WHERE id=22300")
    lab.eventually(lambda: query("secondary", "SELECT ultrafast_multiplier IS NULL FROM channel_model_pricing WHERE id=22300") == "t", timeout=30, label="explicit price reset")
    exercise()
    report["backfill_delta_and_null_reset"] = True
    query("primary", "DELETE FROM channel_model_pricing WHERE id=22300; DELETE FROM channels WHERE id=22300")
    lab.eventually(lambda: query("secondary", "SELECT count(*) FROM channel_model_pricing WHERE id=22300") == "0", timeout=30)

(lab.root / "evidence" / ("upgrade-2230-" + stage + ".json")).write_text(json.dumps(report, indent=2))
print(json.dumps(report))
