#!/usr/bin/env python3
"""Bounded, read-only site observations plus explicit secondary model probes.

Only model probes create ordinary billable application requests. This program
does not mutate application configuration, inject faults or restart containers.
Python 3.11+, standard library only. See README.md for the observation limits.
"""
import argparse
import collections
import datetime as dt
import fcntl
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import socket
import sqlite3
import subprocess
import threading
import time
import urllib.parse
import urllib.request
import uuid

MODEL = "deepseek-flash-local"
THRESHOLDS = (30, 60, 300, 1020, 1200)
LOG_FIELDS = set("time level msg node node_id error reason duration_ms full rows events pending upload_json_bytes upload_wire_bytes download_json_bytes download_wire_bytes result phase protocol connection remote connection_reused failed_pool next_pool count accepted acknowledged rejected request_id".split())
ACCESS_FIELDS = set("time remote_addr method uri status bytes request_time upstream_connect_time upstream_header_time upstream_response_time upstream_status protocol request_id".split())


def utc(ts=None):
    return dt.datetime.fromtimestamp(time.time() if ts is None else ts, dt.timezone.utc).isoformat()


def timestamp(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def scrub(value):
    text = str(value)
    text = re.sub(r"sk-[A-Za-z0-9_-]+", "[redacted-key]", text)
    text = re.sub(r"(?i)Bearer\s+[^\s\"\\]+", "Bearer [redacted]", text)
    text = re.sub(r"(?i)(token|password|api_key)=([^&\s]+)", r"\1=[redacted]", text)
    return text[:2000]


def atomic_json(path, value):
    temp = path.with_suffix(path.suffix + ".tmp")
    with temp.open("w") as f:
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.replace(temp, path)


class Evidence:
    def __init__(self, directory):
        self.root = Path(directory)
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = threading.RLock()
        self.db = sqlite3.connect(self.root / "evidence.sqlite3", check_same_thread=False)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("PRAGMA synchronous=FULL")
        self.db.executescript("""
        CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY, at REAL NOT NULL,
          kind TEXT NOT NULL, unique_key TEXT UNIQUE, data TEXT NOT NULL);
        CREATE INDEX IF NOT EXISTS events_kind_at ON events(kind,at);
        CREATE TABLE IF NOT EXISTS state(key TEXT PRIMARY KEY, value TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS billing(request_id TEXT PRIMARY KEY, data TEXT NOT NULL);
        """)

    def add(self, kind, data, at=None, unique=None):
        with self.lock, self.db:
            cur = self.db.execute("INSERT OR IGNORE INTO events(at,kind,unique_key,data) VALUES(?,?,?,?)",
                                  (time.time() if at is None else at, kind, unique, json.dumps(data, ensure_ascii=False)))
            return cur.rowcount == 1

    def get(self, key, default=None):
        with self.lock:
            row = self.db.execute("SELECT value FROM state WHERE key=?", (key,)).fetchone()
        return default if row is None else json.loads(row[0])

    def put(self, key, value):
        with self.lock, self.db:
            self.db.execute("INSERT INTO state VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                            (key, json.dumps(value)))

    def rows(self, kind):
        with self.lock:
            return [(at, json.loads(data)) for at, data in self.db.execute(
                "SELECT at,data FROM events WHERE kind=? ORDER BY at,id", (kind,))]


def parse_docker_line(raw):
    """Accept Docker's outer timestamp and both JSON and console zap encoders."""
    outer, sep, line = raw.partition(" ")
    if not sep:
        return None
    try:
        at = timestamp(outer)
    except (ValueError, TypeError):
        return None
    try:
        obj = json.loads(line)
    except ValueError:
        fields = line.rstrip().split("\t")
        if len(fields) < 4:
            return {"at": at, "outer": outer, "record": None}
        try:
            obj = json.loads(fields[-1])
            obj.update(time=fields[0], level=fields[1], msg=fields[-2])
        except (ValueError, TypeError):
            return {"at": at, "outer": outer, "record": None}
    if not isinstance(obj, dict):
        obj = None
    return {"at": at, "outer": outer, "record": obj}


class StreamResult:
    def __init__(self, marker):
        self.marker = marker
        self.pending = b""
        self.terminal = None
        self.response_status = None
        self.incomplete_reason = None
        self.errors = []
        self.text = ""
        self.usage = None
        self.first_content = None
        self.first_text = None
        self.started = time.monotonic()

    def feed(self, chunk):
        self.pending += chunk
        self.pending = self.pending.replace(b"\r\n", b"\n")
        if len(self.pending) > 2 * 1024 * 1024:
            raise ValueError("SSE frame exceeded observation limit")
        while b"\n\n" in self.pending:
            frame, self.pending = self.pending.split(b"\n\n", 1)
            self.event(frame)

    def event(self, frame):
        data = b"\n".join(line[5:].lstrip(b" ") for line in frame.split(b"\n") if line.startswith(b"data:"))
        if not data or data == b"[DONE]":
            return
        item = json.loads(data)
        kind = item.get("type", "")
        delta = item.get("delta")
        if kind.endswith(".delta") and isinstance(delta, str) and delta:
            if "reasoning" in kind or kind == "response.output_text.delta":
                self.first_content = self.first_content or time.monotonic() - self.started
            if kind == "response.output_text.delta":
                self.first_text = self.first_text or time.monotonic() - self.started
                self.text = (self.text + delta)[:32768]
        if kind in ("response.completed", "response.failed", "response.incomplete"):
            self.terminal = kind
            response = item.get("response", {})
            self.response_status = response.get("status")
            self.usage = response.get("usage")
            self.incomplete_reason = (response.get("incomplete_details") or {}).get("reason")
            if kind != "response.completed":
                self.errors.append({"type": kind, "error": scrub(response.get("error") or response.get("incomplete_details"))})
        if kind == "error":
            self.errors.append({"type": kind, "error": scrub(item.get("error") or item.get("message") or item.get("code"))})

    def finish(self, eof):
        if self.pending.strip():
            self.event(self.pending)
            self.pending = b""
        return {"terminal": self.terminal, "response_status": self.response_status,
                "incomplete_reason":self.incomplete_reason,
                "eof": eof, "errors": self.errors, "marker_found": self.marker in self.text,
                "output_characters": len(self.text), "usage": self.usage,
                "first_content_s": self.first_content, "first_text_s": self.first_text,
                "success": eof and self.terminal == "response.completed" and self.response_status == "completed"
                and not self.errors and self.marker in self.text}


def model_request(config, key, sequence, timeout=90):
    started = time.time()
    client_id = str(uuid.uuid4())
    marker = "SITE_SOAK_" + client_id.replace("-", "")[:16].upper()
    large = sequence % 40 == 0
    context = ("Synthetic transport context, no external tools required. " * 640) if large else ""
    prompt = context + "\nReturn exactly " + marker + " on the first line, then the integers from 1 to 16 separated by spaces on the second line. No explanation or tools."
    body = json.dumps({"model": MODEL, "input": prompt, "reasoning": {"effort": "high"},
                       "max_output_tokens": 1536, "stream": True, "store": False}).encode()
    url = urllib.parse.urlsplit(config["secondary_url"])
    assert url.scheme == "http" and url.hostname in ("127.0.0.1", "localhost"), "probe must target the local secondary"
    conn = http.client.HTTPConnection(url.hostname, url.port or 80, timeout=min(10, timeout))
    parsed = StreamResult(marker)
    result = {"sequence": sequence, "model": MODEL, "started_at": utc(started), "client_request_id": client_id,
              "profile": "32KiB_context" if large else "short", "probe_revision":2, "request_bytes": len(body)}
    eof = False
    timer = None
    try:
        conn.connect()
        sock = conn.sock
        def cancel():
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        timer = threading.Timer(max(.01, timeout - (time.time() - started)), cancel)
        timer.daemon = True
        timer.start()
        sock.settimeout(min(30, timeout))
        headers = {"Authorization": "Bearer " + key, "Content-Type": "application/json", "Accept": "text/event-stream",
                   "User-Agent": "sub2api-site-soak/1", "X-Client-Request-Id": client_id,
                   "session_id": config["run_id"], "conversation_id": config["run_id"]}
        conn.request("POST", "/v1/responses", body=body, headers=headers)
        response = conn.getresponse()
        result.update(http_status=response.status, request_id=response.getheader("X-Request-ID"),
                      assigned_client_request_id=response.getheader("X-Client-Request-ID"))
        if response.status != 200:
            parsed.errors.append({"http_status": response.status, "detail": scrub(response.read(2048).decode(errors="replace"))})
        else:
            received = 0
            while True:
                if time.time() - started > timeout:
                    raise TimeoutError("total request deadline")
                chunk = response.read1(65536)
                if not chunk:
                    if response.length not in (None, 0):
                        raise http.client.IncompleteRead(b"", response.length)
                    eof = True
                    break
                received += len(chunk)
                if received > 16 * 1024 * 1024:
                    raise ValueError("response exceeded observation limit")
                parsed.feed(chunk)
    except Exception as error:
        parsed.errors.append({"exception": type(error).__name__, "message": scrub(error)})
    finally:
        if timer:
            timer.cancel()
        conn.close()
    try:
        result.update(parsed.finish(eof))
    except Exception as error:
        result.update(success=False, eof=eof, errors=parsed.errors + [{"parse_error": scrub(error)}])
    result["success"] = bool(result.get("success") and result.get("http_status") == 200)
    result["failure_category"] = failure_category(result)
    result["total_s"] = round(time.time() - started, 4)
    return result


def failure_category(result):
    if result.get("success"):
        return None
    if result.get("terminal") == "response.incomplete" and (result.get("incomplete_reason") in ("length","max_output_tokens")
            or any("'reason': 'length'" in str(e.get("error")) for e in result.get("errors",[]))):
        return "output_budget_exhausted"
    if result.get("http_status",200) != 200:
        return "http_error"
    if any("exception" in e for e in result.get("errors",[])):
        return "transport_or_parse_error"
    if result.get("terminal") in ("response.failed","response.incomplete") or result.get("errors"):
        return "stream_error"
    if result.get("terminal") != "response.completed" or not result.get("eof"):
        return "missing_completion"
    return "marker_mismatch"


class Monitor:
    def __init__(self, config, root):
        self.cfg, self.root = config, Path(root)
        assert re.fullmatch(r"[a-zA-Z0-9_-]{1,100}", config["run_id"])
        assert config["role"] in ("primary", "secondary")
        assert config["model"] == MODEL
        self.start, self.end = timestamp(config["start_at"]), timestamp(config["end_at"])
        assert 0 < self.end - self.start <= 36 * 3600 + 1
        self.observation_end = self.end + config.get("settlement_grace_s", 120)
        self.app = "sub2api-bosc-secondary" if config["role"] == "secondary" else "sub2api"
        self.e = Evidence(root)
        self.stop = threading.Event()
        self.threads = []
        self.procs = set()
        self.proc_lock = threading.Lock()
        self.key = ""

    def command(self, args, timeout=12):
        p = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
        if p.returncode:
            raise RuntimeError("command failed: " + args[0] + ": " + scrub(p.stderr))
        return p.stdout

    def sql(self, query):
        return json.loads(self.command(["docker", "exec", self.app + "-postgres", "sh", "-c",
            'export PGOPTIONS="-c default_transaction_read_only=on -c statement_timeout=5000 -c lock_timeout=1000"; exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"',
            "sh", query], timeout=10))

    def periodic(self, name, interval, fn):
        deadline = self.end if name == "requests" else self.observation_end
        if name == "requests" and time.time() < self.start:
            self.stop.wait(self.start - time.time())
        while not self.stop.is_set() and time.time() < deadline:
            begin = time.monotonic()
            try:
                fn()
                self.e.put("worker:" + name, {"at": time.time(), "ok": True})
            except Exception as error:
                self.e.add("collector_error", {"worker": name, "error": scrub(error)})
                self.e.put("worker:" + name, {"at": time.time(), "ok": False, "error": scrub(error)})
            self.stop.wait(max(.1, interval - (time.monotonic() - begin)))

    def alert(self, key, active, detail):
        old = self.e.get("alert:" + key, False)
        if bool(active) != old:
            self.e.add("alert", {"name": key, "active": bool(active), "detail": detail})
            self.e.put("alert:" + key, bool(active))
            print(json.dumps({"at": utc(), "alert": key, "active": bool(active), "detail": detail}), flush=True)

    def sync_event(self, at, record, unique):
        if not self.e.add("site_log", record, at, unique):
            return
        if record.get("msg") == "site synchronization completed":
            previous = self.e.get("last_sync")
            if previous and at <= previous["at"]:
                return
            if previous:
                gap = at - previous["at"]
                self.e.put("max_sync_gap_s", max(gap, self.e.get("max_sync_gap_s", 0)))
                if gap >= THRESHOLDS[0]:
                    self.e.add("sync_gap", {"from": utc(previous["at"]), "to": utc(at), "seconds": gap,
                                             "note": "No observed successful installation; correlate collector coverage and primary access evidence."}, at)
            self.e.put("last_sync", {"at": at, "duration_ms": record.get("duration_ms", 0)})

    def docker_logs(self):
        cursor = self.e.get("docker_cursor", max(self.start - 30, time.time() - 30))
        persisted_at = 0
        while not self.stop.is_set() and time.time() < self.observation_end:
            args = ["docker", "logs", "--timestamps", "--follow", "--since", utc(cursor - 2), self.app]
            p = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            with self.proc_lock:
                self.procs.add(p)
            selector = selectors.DefaultSelector()
            selector.register(p.stdout, selectors.EVENT_READ)
            pending = b""
            try:
                while not self.stop.is_set() and time.time() < self.observation_end:
                    ready = selector.select(1)
                    if time.monotonic() - persisted_at >= 5:
                        self.e.put("worker:docker_logs", {"at": time.time(), "ok": p.poll() is None})
                        self.e.put("docker_cursor", cursor)
                        persisted_at = time.monotonic()
                    if not ready:
                        if p.poll() is not None:
                            break
                        continue
                    data = os.read(p.stdout.fileno(), 65536)
                    if not data:
                        break
                    pending += data
                    if len(pending) > 2 * 1024 * 1024:
                        self.e.add("collector_error", {"worker": "docker_logs", "error": "oversized log line skipped"})
                        pending = b""
                    while b"\n" in pending:
                        line, pending = pending.split(b"\n", 1)
                        parsed = parse_docker_line(line.decode(errors="replace"))
                        if not parsed:
                            continue
                        cursor = max(cursor, parsed["at"])
                        record = parsed["record"]
                        if record is None and b"site synchronization" in line:
                            self.e.add("collector_error", {"worker":"docker_logs", "error":"site log could not be decoded"})
                        if record and (str(record.get("msg", "")).startswith("site ") or str(record.get("level", "")).upper() in ("PANIC", "FATAL")):
                            clean = {k: scrub(v) if isinstance(v, str) else v for k, v in record.items() if k in LOG_FIELDS}
                            self.sync_event(parsed["at"], clean, "log:" + hashlib.sha256(line).hexdigest())
            finally:
                self.e.put("docker_cursor", cursor)
                selector.close()
                if p.poll() is None:
                    p.terminate()
                try:
                    p.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
                p.stdout.close()
                with self.proc_lock:
                    self.procs.discard(p)
            if not self.stop.is_set() and time.time() < self.observation_end:
                self.e.add("log_reconnect", {"cursor": utc(cursor), "returncode": p.returncode})
                self.stop.wait(2)

    def health(self):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        result = {}
        for path in ("/health", "/readyz"):
            try:
                with opener.open("http://127.0.0.1:8080" + path, timeout=2) as response:
                    result[path] = response.status
            except Exception as error:
                result[path] = getattr(error, "code", 0)
        self.e.add("health", result)
        self.e.put("last_health", result)
        self.alert("not_ready", result["/readyz"] != 200, result)
        if self.cfg["role"] == "secondary":
            last = self.e.get("last_sync")
            age = time.time() - (last["at"] if last else max(self.start, self.e.get("sync_observation_started", self.start)))
            for seconds in THRESHOLDS:
                self.alert("no_successful_sync_" + str(seconds) + "s", age >= seconds,
                           {"age_s": round(age, 2), "last_success": utc(last["at"]) if last else None,
                            "log_collector": self.e.get("worker:docker_logs")})
        self.e.put("heartbeat", time.time())
        self.write_status()

    def resources(self):
        containers = json.loads(self.command(["docker", "inspect", self.app, self.app + "-postgres", self.app + "-redis"]))
        rows = [{"name": c["Name"], "id": c["Id"], "image": c["Config"]["Image"], "started_at": c["State"]["StartedAt"],
                 "health": c["State"].get("Health", {}).get("Status"), "running": c["State"]["Running"],
                 "oom": c["State"]["OOMKilled"], "restarts": c["RestartCount"]} for c in containers]
        last = self.e.get("containers")
        if last and rows != last:
            self.e.add("container_change", {"before": last, "after": rows})
        self.e.put("containers", rows)
        query = """SELECT json_build_object('time',now(),
          'identity',(SELECT row_to_json(x) FROM (SELECT mode,node_id,primary_id,version FROM site_identity)x),
          'outbox_count_capped',(SELECT count(*) FROM (SELECT id FROM site_outbox LIMIT 10001)x),
          'oldest_outbox_s',(SELECT greatest(0,extract(epoch FROM clock_timestamp()-created_at)) FROM site_outbox ORDER BY id LIMIT 1),
          'unresolved_capped',(SELECT count(*) FROM (SELECT event_id FROM site_rejected_events WHERE resolved_at IS NULL LIMIT 1001)x),
          'db_connections',(SELECT count(*) FROM pg_stat_activity),
          'db_active',(SELECT count(*) FROM pg_stat_activity WHERE state='active'),
          'db_lock_waits',(SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock'))"""
        db = self.sql(query)
        info = {"containers": rows, "db": db, "disk_free": shutil.disk_usage(self.root).free,
                "load": os.getloadavg(), "meminfo": {}}
        for line in Path("/proc/meminfo").read_text().splitlines():
            name, value = line.split(":", 1)
            if name in ("MemAvailable", "MemTotal", "SwapFree", "SwapTotal"):
                info["meminfo"][name] = value.strip()
        self.e.add("resources", info)
        self.e.put("last_resources", info)
        self.alert("billing_backlog_60s", (db["oldest_outbox_s"] or 0) >= 60, db)
        self.alert("billing_rejection", db["unresolved_capped"] > 0, {"unresolved_capped": db["unresolved_capped"]})
        self.alert("disk_low", info["disk_free"] < 1024**3, {"free": info["disk_free"]})

    def primary_probe(self):
        start = time.monotonic()
        # Run inside the app container: same DNS/network namespace, fresh HTTP
        # connection, no synthetic authenticated synchronization request.
        p = subprocess.run(["docker", "exec", self.app, "wget", "-S", "-T", "6", "-O", "/dev/null",
                            self.cfg["primary_url"].rstrip("/") + "/health"], text=True, capture_output=True, timeout=9)
        codes = re.findall(r"HTTP/\S+\s+(\d{3})", p.stderr)
        result = {"ok": p.returncode == 0 and bool(codes) and codes[-1] == "200", "returncode": p.returncode,
                  "http_codes": codes, "seconds": round(time.monotonic() - start, 4),
                  "error": scrub(p.stderr) if p.returncode else None}
        self.e.add("fresh_primary_connection", result)
        self.e.put("last_primary_probe", result)
        self.alert("fresh_primary_connection_failed", not result["ok"], result)

    def billing(self):
        # Narrow to the test's explicit session declaration and authorized key.
        # At most ~8,640 records over 36h, every five minutes, plus final export.
        query = """SELECT coalesce(json_agg(x),'[]') FROM (SELECT id,request_id,account_id,model,
          input_tokens,output_tokens,actual_cost,created_at FROM usage_logs
          WHERE api_key_id=%d AND created_at>='%s'::timestamptz
          AND client_context->>'session_id'='%s' ORDER BY id)x""" % (int(self.cfg["api_key_id"]), self.cfg["start_at"], self.cfg["run_id"])
        rows = self.sql(query)
        with self.e.lock, self.e.db:
            for row in rows:
                self.e.db.execute("INSERT INTO billing VALUES(?,?) ON CONFLICT(request_id) DO UPDATE SET data=excluded.data",
                                  (row["request_id"], json.dumps(row)))
        self.e.add("billing_sample", {"rows": len(rows), "duplicate_request_ids": len(rows) - len({r["request_id"] for r in rows}),
                                      "total_cost": sum(float(r["actual_cost"]) for r in rows)})
        self.e.put("last_billing_count", len(rows))
        self.e.put("last_billing_duplicate_ids", len(rows)-len({r["request_id"] for r in rows}))

    def nginx(self):
        path = Path(self.cfg["nginx_access_log"])
        st = path.stat()
        previous = self.e.get("nginx_position")
        if not previous:
            previous = {"inode": st.st_ino, "offset": st.st_size}
            self.e.add("nginx_observation_start", {"path": str(path), **previous})
        source = path
        if previous["inode"] != st.st_ino:
            rotated = next((p for p in path.parent.glob(path.name + "*")
                            if p.is_file() and p.stat().st_ino == previous["inode"]), None)
            if rotated and rotated.stat().st_size > previous["offset"]:
                source = rotated
            else:
                if not rotated:
                    self.e.add("coverage_gap", {"source":"nginx", "reason":"rotated inode unavailable; last unread old-file bytes may be missing"})
                self.e.add("log_rotated", {"source":"nginx"})
                previous = {"inode":st.st_ino, "offset":0}
        elif previous["offset"] > st.st_size:
            self.e.add("coverage_gap", {"source":"nginx", "reason":"log truncated"})
            previous = {"inode":st.st_ino, "offset":0}
        with source.open("rb") as f:
            f.seek(previous["offset"])
            data = f.read(4 * 1024 * 1024)
        complete, _, _ = data.rpartition(b"\n")
        consumed = len(complete) + 1 if b"\n" in data else 0
        for line in complete.splitlines():
            if b"/internal/sites/" not in line:
                continue
            try:
                row = json.loads(line)
            except ValueError:
                self.e.add("collector_error", {"worker": "nginx", "error": "non-JSON site access line; adjust parser"})
                continue
            row = {k: v for k, v in row.items() if k in ACCESS_FIELDS}
            if row.get("uri", "").split("?", 1)[0] not in ("/internal/sites/sync", "/internal/sites/events"):
                continue
            row["uri"] = row["uri"].split("?", 1)[0]
            self.e.add("primary_access", row, unique="access:" + hashlib.sha256(line).hexdigest())
        self.e.put("nginx_position", {"inode": previous["inode"], "offset": previous["offset"] + consumed})

    def requests(self):
        sequence = self.e.get("request_sequence", 0) + 1
        self.e.put("request_sequence", sequence)
        if self.e.get("alert:disk_low"):
            self.e.add("request_skipped", {"sequence": sequence, "reason": "disk guard"})
            return
        self.e.add("request_started", {"sequence": sequence})
        result = model_request(self.cfg, self.key, sequence, timeout=90)
        self.e.add("request", result)
        self.alert("model_probe_failed", not result["success"], {k: result.get(k) for k in ("sequence", "request_id", "http_status", "terminal", "errors")})

    def write_status(self, final=False):
        now = time.time()
        observed_now = min(now,self.observation_end)
        with self.e.lock:
            counts = dict(self.e.db.execute("SELECT kind,count(*) FROM events GROUP BY kind"))
            successful = self.e.db.execute("SELECT count(*) FROM events WHERE kind='request' AND json_extract(data,'$.success')=1").fetchone()[0]
            active = {k[6:]: json.loads(v) for k, v in self.e.db.execute("SELECT key,value FROM state WHERE key LIKE 'alert:%'") if json.loads(v)}
        last = self.e.get("last_sync")
        sync_age = max(0, observed_now - (last["at"] if last else max(self.start,self.e.get("sync_observation_started",self.start)))) if self.cfg["role"] == "secondary" else None
        maxgap = self.e.get("max_sync_gap_s", 0)
        if sync_age is not None:
            maxgap = max(maxgap, sync_age)
        result = {"run_id": self.cfg["run_id"], "role": self.cfg["role"], "model": MODEL,
                  "start_at": self.cfg["start_at"], "end_at": self.cfg["end_at"], "updated_at": utc(),
                  "phase": ("finished" if now >= self.observation_end else "stopped") if final else ("settling" if now >= self.end else "running"),
                  "remaining_s": max(0, self.end-now), "event_counts": counts, "requests_completed": counts.get("request", 0),
                  "requests_successful": successful, "requests_failed": counts.get("request", 0)-successful,
                  "last_sync": last, "sync_age_s": sync_age, "max_observed_sync_gap_s": maxgap,
                  "estimated_lease_remaining_s": max(0, 1200-sync_age-last.get("duration_ms",0)/1000) if last else None,
                  "lease_note": "Estimate from successful-install logs; /readyz is the current serving authority, restart may invalidate the lease.",
                  "health": self.e.get("last_health"), "active_alerts": active,
                  "primary_fresh_connection": self.e.get("last_primary_probe"),
                  "billing_rows": self.e.get("last_billing_count", 0), "resources": self.e.get("last_resources"),
                  "billing_duplicate_ids":self.e.get("last_billing_duplicate_ids",0),
                  "request_records_without_completion":counts.get("request_started",0)-counts.get("request",0),
                  "monitor_observation_gaps": counts.get("coverage_gap",0), "collector_errors": counts.get("collector_error",0)}
        with self.e.lock:
            result["workers"] = {k[7:]:json.loads(v) for k,v in self.e.db.execute("SELECT key,value FROM state WHERE key LIKE 'worker:%'")}
        atomic_json(self.root / "status.json", result)
        if final:
            self.e.add("monitor_finished", {"phase": result["phase"]})
            atomic_json(self.root / "report.json", self.report(result))
        return result

    def report(self, status=None):
        status = status or json.loads((self.root / "status.json").read_text())
        reqs = [v for _, v in self.e.rows("request")]
        def distribution(key):
            values = sorted(v[key] for v in reqs if v.get(key) is not None)
            return None if not values else {"samples":len(values), "mean":sum(values)/len(values),
                "p50":values[math.ceil(len(values)*.5)-1], "p95":values[math.ceil(len(values)*.95)-1], "max":values[-1]}
        with self.e.lock:
            billing = [json.loads(r[0]) for r in self.e.db.execute("SELECT data FROM billing ORDER BY request_id")]
        samples = self.e.rows('billing_sample')
        return {"status":status, "latency_seconds":{k:distribution(k) for k in ("first_content_s","first_text_s","total_s")},
                "failure_categories":dict(collections.Counter(failure_category(v) for v in reqs if not v.get("success"))),
                "sync_gaps":[v for _,v in self.e.rows("sync_gap")], "alerts":[{"at":utc(t),**v} for t,v in self.e.rows("alert")],
                "failed_requests":[v for v in reqs if not v.get("success")], "billing":billing,
                "billing_sample_at":utc(samples[-1][0]) if samples else None,
                "conclusion_note":"A clean observation window cannot prove an intermittent failure is impossible. Collect both hosts to reconcile imported billing and distinguish missing requests from missing responses."}

    def run(self):
        if time.time() >= self.observation_end and (self.root/'status.json').exists():
            previous = json.loads((self.root/'status.json').read_text())
            if previous.get('phase') == 'finished':
                if self.cfg['role'] == 'secondary':
                    (self.root/'api-key').unlink(missing_ok=True)
                return
        old = self.e.get("heartbeat")
        observed_now = min(time.time(),self.observation_end)
        if old and observed_now - old > 15:
            self.e.add("coverage_gap", {"source":"monitor", "from":utc(old), "to":utc(observed_now), "seconds":observed_now-old})
        self.e.put("process_started", time.time())
        if self.e.get("sync_observation_started") is None:
            self.e.put("sync_observation_started",time.time())
        self.e.add("monitor_started", {"pid":os.getpid(), "deadline":self.cfg["end_at"]})
        if time.time() >= self.observation_end:
            self.write_status(final=True)
            if self.cfg["role"] == "secondary":
                (self.root / "api-key").unlink(missing_ok=True)
            return
        if self.cfg["role"] == "secondary":
            keypath = self.root / "api-key"
            assert keypath.stat().st_mode & 0o077 == 0, "key must be mode 0600"
            self.key = keypath.read_text().strip()
            assert self.key.startswith("sk-") and "\n" not in self.key
        def launch(name, target):
            thread = threading.Thread(name=name, target=target, daemon=True)
            thread.start()
            self.threads.append(thread)
        launch("docker_logs", self.docker_logs)
        tasks = [("health",5,self.health),("resources",30,self.resources),("billing",300,self.billing)]
        if self.cfg["role"] == "secondary":
            tasks += [("primary_probe",30,self.primary_probe),("requests",15,self.requests)]
        else:
            tasks += [("nginx",5,self.nginx)]
        for name, interval, fn in tasks:
            launch(name, lambda n=name,i=interval,f=fn:self.periodic(n,i,f))
        try:
            while not self.stop.wait(1) and time.time() < self.observation_end:
                if (self.root / "STOP").exists():
                    break
                if any(not t.is_alive() and not (t.name == "requests" and time.time() >= self.end) for t in self.threads) and time.time() < self.observation_end - 1:
                    raise RuntimeError("monitor worker exited unexpectedly")
                if shutil.disk_usage(self.root).free < 128 * 1024 * 1024:
                    raise RuntimeError("insufficient space to preserve monitoring evidence")
        finally:
            self.stop.set()
            with self.proc_lock:
                for p in self.procs:
                    if p.poll() is None:
                        p.terminate()
            for thread in self.threads:
                thread.join(timeout=95 if thread.name == "requests" else 12)
        try:
            self.billing()
        except Exception as error:
            self.e.add("collector_error", {"worker":"final_billing", "error":scrub(error)})
        self.write_status(final=True)
        if time.time() >= self.observation_end and self.cfg["role"] == "secondary":
            (self.root / "api-key").unlink(missing_ok=True)


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("run", "report"))
    parser.add_argument("--directory", required=True)
    args = parser.parse_args()
    directory = Path(args.directory).resolve()
    cfg = json.loads((directory / "config.json").read_text())
    monitor = Monitor(cfg, directory)
    if args.command == "report":
        print(json.dumps(monitor.report(), ensure_ascii=False, indent=2))
        return
    lock = (directory / "run.lock").open("a")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    signal.signal(signal.SIGTERM, lambda *_:monitor.stop.set())
    signal.signal(signal.SIGINT, lambda *_:monitor.stop.set())
    monitor.run()


if __name__ == "__main__":
    main()
