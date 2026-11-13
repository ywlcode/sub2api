"""Exercise primary reconciliation APIs and replica balances in a mock lab."""
import json
import sys
from decimal import Decimal

from lab import Lab

lab = Lab(sys.argv[1])
assert lab.sql("primary", "SELECT name FROM accounts WHERE id=101;") == "domestic", "requires the synthetic mock lab"
lab.eventually(lab.settled, label="initial billing settlement")
# Synthetic compliance fixture, not a real operator acknowledgement.
lab.sql("primary", """INSERT INTO settings(key,value)
SELECT 'admin_compliance_acknowledgement:'||id,
json_build_object('version','v2026.06.10','admin_user_id',id,'acknowledged_at',now())::text
FROM users WHERE email='primary@site-lab.invalid'
ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value;""")
login = lab.http("primary", "/api/v1/auth/login", {"email": "primary@site-lab.invalid", "password": lab.creds["admin"]})
assert login["status"] == 200
headers = {"Authorization": "Bearer " + json.loads(login["body"])["data"]["access_token"]}
base = "/api/v1/admin/sites/lab-secondary/rejections"
evidence = []


def balance(site):
    return Decimal(lab.sql(site, "SELECT balance FROM users WHERE id=101;"))


def reject_bill(retryable):
    lab.control(mode="reject")
    try:
        result = lab.request(stream=True)
        assert result["status"] == 200 and "response.completed" in result["body"], result
        event = lab.query("secondary", "SELECT id,payload FROM site_outbox WHERE kind='billing' ORDER BY id DESC LIMIT 1")[0]
        event_id = int(event["id"])
        change = "jsonb_set(payload,'{usage,CreatedAt}',to_jsonb(to_char(NOW()+INTERVAL '1 hour','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')))" if retryable else "jsonb_set(payload,'{version}','0'::jsonb)"
        lab.sql("secondary", f"UPDATE site_outbox SET payload={change} WHERE id={event_id};")
    finally:
        lab.control(mode="normal")
    state = "retrying" if retryable else "pending"
    lab.eventually(lambda: lab.sql("primary", f"SELECT state FROM site_rejected_events WHERE node_id='lab-secondary' AND event_id={event_id};") == state, label="primary rejection")
    lab.eventually(lambda: lab.sql("secondary", f"SELECT state FROM site_rejected_events WHERE event_id={event_id};") == state, label="secondary rejection")
    return event_id, Decimal(str(event["payload"]["command"]["BalanceCost"]))


event_id, cost = reject_bill(False)
listed = lab.http("primary", base + "?state=pending", headers=headers)
assert listed["status"] == 200
assert any(row["event_id"] == event_id and "payload" not in row for row in json.loads(listed["body"])["data"])
detail = lab.http("primary", f"{base}/{event_id}", headers=headers)
assert detail["status"] == 200 and json.loads(detail["body"])["data"]["payload"]["version"] == 0
before = balance("primary")
# Simulate an independently completed manual primary adjustment.
lab.sql("primary", f"UPDATE users SET balance=balance-{cost} WHERE id=101;")
lab.eventually(lambda: balance("secondary") == before - cost * 2, label="manual charge plus unresolved reservation")
for _ in range(2):
    resolved = lab.http("primary", f"{base}/{event_id}/resolve", {"action": "settled", "note": "Lab: manual debit verified"}, headers)
    assert resolved["status"] == 200, resolved
lab.eventually(lambda: balance("secondary") == balance("primary") == before - cost, label="reservation removed atomically")
conflict = lab.http("primary", f"{base}/{event_id}/resolve", {"action": "waived", "note": "Lab: conflicting repeat"}, headers)
assert conflict["status"] == 409, conflict
evidence.append({"case": "manual_settlement", "event_id": event_id, "cost": str(cost), "primary_balance": str(balance("primary")), "secondary_balance": str(balance("secondary")), "repeat_status": 200, "conflict_status": conflict["status"]})

event_id, cost = reject_bill(True)
before = balance("primary")
blocked = lab.http("primary", f"{base}/{event_id}/resolve", {"action": "retry", "note": "Lab: timestamp still in future"}, headers)
assert blocked["status"] == 409, blocked
resolved = lab.http("primary", f"{base}/{event_id}/resolve", {"action": "waived", "note": "Lab: explicit waiver"}, headers)
assert resolved["status"] == 200, resolved
lab.eventually(lambda: balance("secondary") == balance("primary") == before and lab.settled(), label="waiver removes deferred reservation")
evidence.append({"case": "deferred_waiver", "event_id": event_id, "retry_status": blocked["status"], "primary_balance": str(balance("primary")), "secondary_balance": str(balance("secondary"))})
unauthorized = lab.http("primary", base)
assert unauthorized["status"] == 401
evidence.append({"case": "admin_authorization", "anonymous_status": unauthorized["status"]})
path = lab.root / "evidence/reconciliation.json"
path.write_text(json.dumps(evidence, indent=2))
print(json.dumps({"passed": True, "results": evidence, "evidence": str(path)}), flush=True)
