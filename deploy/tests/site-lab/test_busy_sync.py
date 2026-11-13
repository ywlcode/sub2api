"""Keep billing, delivery and policy export active across multiple leases."""
import concurrent.futures
import json
import sys
import time
from decimal import Decimal

from lab import Lab

lab = Lab(sys.argv[1])
assert lab.sql("primary", "SELECT name FROM accounts WHERE id=101;") == "domestic", "requires synthetic mock lab"
lab.eventually(lab.settled, label="initial settlement")
before = Decimal(lab.sql("primary", "SELECT balance FROM users WHERE id=101;"))
usage_before = Decimal(lab.sql("primary", "SELECT COALESCE(SUM(actual_cost),0) FROM usage_logs WHERE user_id=101;"))
initial_records = len(lab.records())
samples = []


def writer():
    # psql executes each statement in its own transaction. Four long-running
    # writers repeatedly update the same account owner without holding one
    # artificial transaction open for the entire experiment.
    lab.sql("primary", "UPDATE users SET balance=balance-0.00000001 WHERE id=101; SELECT pg_sleep(0.025);\n" * 1000)


def requester():
    for _ in range(50):
        result = lab.request(stream=True)
        assert result["status"] == 200 and "response.completed" in result["body"], result
        time.sleep(0.4)


started = time.monotonic()
with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
    pending = [pool.submit(writer) for _ in range(4)] + [pool.submit(requester) for _ in range(8)]
    while not all(task.done() for task in pending):
        status = lab.http("secondary", "/readyz")["status"]
        samples.append({"elapsed": round(time.monotonic() - started, 3), "status": status})
        assert status == 200, samples[-1]
        time.sleep(0.5)
    for task in pending:
        task.result()
lab.eventually(lab.settled, timeout=40, label="busy upload drained")
usage_after = Decimal(lab.sql("primary", "SELECT COALESCE(SUM(actual_cost),0) FROM usage_logs WHERE user_id=101;"))
expected = before - Decimal("0.00004000") - (usage_after - usage_before)
for node in ("primary", "secondary"):
    lab.eventually(lambda: Decimal(lab.sql(node, "SELECT balance FROM users WHERE id=101;")) == expected, label=node + " balance")
syncs = [r for r in lab.records()[initial_records:] if r.get("kind") == "sync"]
assert len(syncs) >= 4, syncs
assert all(r.get("status") == 200 for r in syncs), syncs
result = {"passed": True, "primary_debits": 4000, "secondary_stream_requests": 400,
          "seconds": round(time.monotonic() - started, 3), "syncs": len(syncs),
          "ready_samples": samples, "final_balance": str(expected)}
path = lab.root / "evidence/busy-sync.json"
path.write_text(json.dumps(result, indent=2))
print(json.dumps(result), flush=True)
