"""Synthetic 400-user comparison. Use only a dedicated, seeded mock site lab."""
import argparse
import concurrent.futures
import http.client
import json
import random
import secrets
import statistics
import subprocess
import threading
import time
from urllib.parse import urlsplit

from lab import Lab

parser = argparse.ArgumentParser()
parser.add_argument("directory")
parser.add_argument("stage")
parser.add_argument("--concurrency", type=int, default=200)
parser.add_argument("--requests", type=int, default=400)
parser.add_argument("--seed", action="store_true")
parser.add_argument("--routes", nargs="+", choices=("primary", "secondary_local", "secondary_gzip"), default=["primary", "secondary_local", "secondary_gzip"])
parser.add_argument("--warmup", action="store_true", help="warm all 400 identities before each measured route")
args = parser.parse_args()
lab = Lab(args.directory)
assert "rebase" in lab.root.name or "rebase" in str(lab.root.parent), "use a dedicated rebase mock lab"
keys_file = lab.root / "benchmark-users.json"
if args.seed:
    assert not keys_file.exists(), "seed once; preserve identical old/new data"
    users = [{"id": 20000+i, "key": "sk-bench-"+secrets.token_hex(24)} for i in range(400)]
    sql = ["BEGIN;"]
    for u in users:
        sql.append(f"INSERT INTO users(id,email,password_hash,balance,concurrency) VALUES({u['id']},'bench-{u['id']}@site-lab.invalid','disabled',10000,4);")
        sql.append(f"INSERT INTO api_keys(id,user_id,key,name,group_id) VALUES({u['id']},{u['id']},'{u['key']}','benchmark',101);")
    sql += ["UPDATE accounts SET concurrency=512 WHERE id IN (101,102);", "COMMIT;"]
    lab.sql("primary", "\n".join(sql))
    keys_file.write_text(json.dumps(users))
    keys_file.chmod(0o600)
    lab.eventually(lambda: lab.sql("secondary", "SELECT count(*) FROM api_keys WHERE id>=20000 AND id<20400") == "400", timeout=60)
    lab.command("restart", "primary", "secondary")
    lab.urls.clear()
    lab.eventually(lab.ready, timeout=60)
users = json.loads(keys_file.read_text())
out = lab.root / "evidence" / ("benchmark-"+args.stage)
out.mkdir(exist_ok=False)
words = "time day man world life hand part child eye woman place work week case point government company number group problem fact home water room mother area money story month lot right study book job word business issue side kind head house service friend father power hour game line end member law car city community name president team minute idea kid body information back parent face others level office door health person art war history party result change morning reason research girl guy moment air teacher force education foot boy age policy process music market sense nation plan college interest death experience effect use class control care field development role effort rate heart drug show leader light voice wife police mind price report decision son view relationship town road arm difference value building action model season society tax director position player record paper space ground form event official matter center couple site project activity star table need court oil situation cost industry figure street image phone data picture practice piece land product doctor wall patient worker news test movie north love support technology step baby computer type attention film tree source organization hair look century evidence window culture chance brother energy period course summer plant opportunity term letter condition choice rule daughter administration south husband floor campaign material population economy medical hospital church risk fire future defense security bank sport board subject officer rest behavior performance top goal second bed order author blood agency nature color store sound movement page race concern series language response animal factor decade article east artist scene stock career treatment approach size dog fund media sign thought list individual quality pressure answer resource meeting disease success cup amount ability staff character growth loss degree wonder attack region television box training trade deal election evening audience standard management network request history token thinking".split()
rng = random.Random(2215)
context = " ".join(rng.choices(words, k=100000))
stop = threading.Event()
samples = []
services = [lab.command("ps", "-q", s).stdout.strip() for s in ("primary", "secondary")]

def monitor():
    while not stop.is_set():
        sample = {"at": time.time()}
        try:
            stats = subprocess.check_output(["docker", "stats", "--no-stream", "--format", "{{json .}}", *services], text=True)
            sample["containers"] = [json.loads(line) for line in stats.splitlines()]
            sample["outbox"] = int(lab.sql("secondary", "SELECT count(*) FROM site_outbox"))
            sample["ready"] = lab.ready()
            sample["db"] = lab.query("primary", "SELECT xact_commit,xact_rollback,blks_hit,blks_read FROM pg_stat_database WHERE datname='sub2api'")
        except Exception as exc:
            sample["error"] = type(exc).__name__
        samples.append(sample)
        stop.wait(1)

thread = threading.Thread(target=monitor)
thread.start()
summary = {"concurrency": args.concurrency, "requests_per_route": args.requests, "users": len(users), "input_words": 100000, "warmup": args.warmup,
           "output_budget": 3000, "upstream": "deterministic mock: 1.5 second delay, 3000 word output; usage token counts simulated", "routes": {}}
try:
    for route, site, model in (("primary", "primary", "deepseek-chat"), ("secondary_local", "secondary", "deepseek-chat"), ("secondary_gzip", "secondary", "gpt-test")):
        if route not in args.routes:
            continue
        if args.warmup:
            def warm(user):
                reply = lab.request(model=model, site=site, key=user["key"], text="warm identity")
                assert reply["status"] == 200, (route, user["id"], reply)
            with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
                list(pool.map(warm, users))
            lab.eventually(lab.settled, timeout=90, label="warmup bills settled")
        endpoint = urlsplit(lab.url(site))
        def database_totals():
            return {name: lab.query(name, "SELECT sum(calls)::bigint AS calls, sum(total_exec_time) AS execution_ms FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())")[0]
                    for name in ("primary", "secondary")}
        # Optional pg_stat_statements must be enabled in both isolated DBs.
        have_statements = all(lab.sql(name, "SELECT count(*) FROM pg_extension WHERE extname='pg_stat_statements'") == "1" for name in ("primary", "secondary"))
        db_before = database_totals() if have_statements else None
        payload = json.dumps({"model": model, "stream": True, "input": "LAB_BENCH "+context, "reasoning": {"effort": "high"}, "max_output_tokens": 3000}, separators=(",", ":")).encode()
        rows = []
        lock = threading.Lock()
        gate = threading.Barrier(args.concurrency)
        state = {"next": 0, "active": 0, "peak": 0}
        def request(i):
            row = {"i": i, "route": route, "user_id": users[i % len(users)]["id"], "body_bytes": len(payload), "at": time.time(), "first_content_s": None, "terminal": []}
            conn = http.client.HTTPConnection(endpoint.hostname, endpoint.port, timeout=120)
            start = time.monotonic()
            try:
                conn.request("POST", "/v1/responses", payload, {"Authorization": "Bearer "+users[i % len(users)]["key"], "Content-Type": "application/json", "X-Client-Request-ID": f"bench-{args.stage}-{route}-{i}", "Connection": "close"})
                row["upload_s"] = time.monotonic()-start
                resp = conn.getresponse()
                row.update(status=resp.status, headers_s=time.monotonic()-start, request_id=resp.getheader("X-Request-ID"))
                if resp.status != 200:
                    row["error"] = resp.read(2048).decode(errors="replace")
                else:
                    for line in resp:
                        if not line.startswith(b"data:"): continue
                        raw = line[5:].strip()
                        if not raw or raw == b"[DONE]": continue
                        event = json.loads(raw)
                        kind = event.get("type", "")
                        if kind in ("response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta") and event.get("delta") and row["first_content_s"] is None:
                            row["first_content_s"] = time.monotonic()-start
                        if kind in ("response.completed", "response.failed", "response.incomplete", "error"):
                            row["terminal"].append(kind)
                row["ok"] = row.get("status") == 200 and row["terminal"] == ["response.completed"] and row["first_content_s"] is not None
            except Exception as exc:
                row.update(ok=False, error=str(exc)[:500])
            finally:
                conn.close()
                row["seconds"] = time.monotonic()-start
                with lock:
                    rows.append(row)
                    state["active"] -= 1
            return row
        def worker():
            gate.wait(timeout=30)
            while True:
                with lock:
                    i = state["next"]
                    if i >= args.requests: return
                    state["next"] += 1
                    state["active"] += 1
                    state["peak"] = max(state["peak"], state["active"])
                request(i)
        begun = time.monotonic()
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            for future in [pool.submit(worker) for _ in range(args.concurrency)]: future.result()
        result = {"ok": sum(r["ok"] for r in rows), "total": len(rows), "peak": state["peak"], "wall_s": time.monotonic()-begun}
        for field in ("upload_s", "first_content_s", "seconds"):
            values = sorted(r[field] for r in rows if r.get(field) is not None)
            result[field] = {"mean": statistics.mean(values), "p50": values[len(values)//2], "p95": values[int(len(values)*.95)-1], "p99": values[int(len(values)*.99)-1]} if values else None
        summary["routes"][route] = result
        (out / (route+".json")).write_text(json.dumps(rows, indent=2))
        print(json.dumps({"route": route, **result}), flush=True)
        lab.eventually(lab.settled, timeout=90, label="billing caught up between routes")
        if db_before is not None:
            db_after = database_totals()
            result["database"] = {name: {field: (db_after[name][field] or 0) - (db_before[name][field] or 0)
                                         for field in ("calls", "execution_ms")}
                                  for name in db_before}
finally:
    stop.set()
    thread.join(timeout=30)
    (out / "samples.json").write_text(json.dumps(samples, indent=2))
    (out / "summary.json").write_text(json.dumps(summary, indent=2))
assert all(r["ok"] == r["total"] for r in summary["routes"].values()), "inspect every stream failure in raw evidence"
