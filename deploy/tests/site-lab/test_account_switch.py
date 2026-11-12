"""Test live account grants using the admin API in a disposable mock lab."""
import concurrent.futures
import json
import sys
import time
import urllib.request

from lab import Lab

lab = Lab(sys.argv[1])
assert lab.sql("primary", "SELECT name FROM accounts WHERE id=101;") == "domestic", "requires a mock lab"
# Synthetic admin fixture, as in the service's compliance middleware tests.
# This is not an acknowledgement by a real operator or a production change.
lab.sql("primary", """INSERT INTO settings(key,value)
SELECT 'admin_compliance_acknowledgement:'||id,
json_build_object('version','v2026.06.10','admin_user_id',id,'acknowledged_at',now())::text
FROM users WHERE email='primary@site-lab.invalid'
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value;""")
login = lab.http("primary", "/api/v1/auth/login", {"email": "primary@site-lab.invalid", "password": lab.creds["admin"]})
assert login["status"] == 200
headers = {"Authorization": "Bearer " + json.loads(login["body"])["data"]["access_token"], "Content-Type": "application/json"}
evidence = []


def save(payload):
    req = urllib.request.Request(lab.url("primary") + "/api/v1/admin/accounts/101", data=json.dumps(payload).encode(), headers=headers, method="PUT")
    with lab.opener.open(req, timeout=20) as response:
        assert response.status == 200
        return json.load(response)["data"]


def account():
    response = lab.http("primary", "/api/v1/admin/accounts/101", headers=headers)
    assert response["status"] == 200
    return json.loads(response["body"])["data"]


def local_count():
    return int(lab.sql("secondary", "SELECT count(*) FROM usage_logs;"))


def success(result, protocol="responses"):
    assert result["status"] == 200, result
    assert "lab answer" in result["body"], result
    terminal = {"responses": "response.completed", "messages": "message_stop", "chat/completions": "[DONE]"}[protocol]
    assert terminal in result["body"], result


before = account()
for enabled in (True, False, True):
    current = account()
    started = time.monotonic()
    save({"extra": {**(current.get("extra") or {}), "site_secondary_enabled": enabled}})
    expected = "1" if enabled else "0"
    lab.eventually(lambda: lab.sql("secondary", "SELECT count(*) FROM accounts WHERE id=101 AND deleted_at IS NULL;") == expected and lab.ready(), timeout=20, label="live account grant")
    sync_seconds = round(time.monotonic() - started, 3)
    after = account()
    assert (before["status"], before["schedulable"], before["group_ids"]) == (after["status"], after["schedulable"], after["group_ids"])
    initial = local_count()
    for protocol in ("responses", "chat/completions", "messages"):
        success(lab.request(protocol=protocol, site="primary", stream=True), protocol)
        success(lab.request(protocol=protocol, site="secondary", stream=True), protocol)
    lab.eventually(lab.settled, timeout=30, label="usage upload")
    local_delta = local_count() - initial
    assert local_delta == (3 if enabled else 0), (enabled, local_delta)
    result = {"enabled": enabled, "sync_seconds": sync_seconds, "primary_direct_success": 3, "secondary_ingress_success": 3, "secondary_local_bills": local_delta}
    evidence.append(result)
    print(json.dumps(result), flush=True)

# Updating the ordinary account model configuration needs no second routing list.
current = account()
credentials = current["credentials"]
credentials["model_mapping"]["deepseek-extra"] = "deepseek-chat"
save({"credentials": credentials})
lab.eventually(lambda: lab.sql("secondary", "SELECT credentials->'model_mapping'->>'deepseek-extra' FROM accounts WHERE id=101 AND deleted_at IS NULL;") == "deepseek-chat", timeout=20, label="new account model")
initial = local_count()
with concurrent.futures.ThreadPoolExecutor(max_workers=5) as pool:
    results = list(pool.map(lambda index: lab.request(model="deepseek-extra", site="primary" if index % 2 else "secondary", stream=True), range(10)))
for result in results:
    success(result)
lab.eventually(lab.settled, timeout=30, label="concurrent usage upload")
assert local_count() - initial == 5
evidence.append({"new_account_model": "deepseek-extra", "concurrency": 5, "success": len(results), "secondary_local_bills": 5})

initial = local_count()
success(lab.request(model="gpt-test", stream=True, text="gzip forwarding " * 200))
assert local_count() == initial, "foreign requests must only bill on the primary"
evidence.append({"gpt_forwarded_success": True, "gpt_secondary_bills": 0})
path = lab.root / "evidence/account-switch.json"
path.write_text(json.dumps(evidence, indent=2))
print(json.dumps({"passed": True, "evidence": str(path)}), flush=True)
