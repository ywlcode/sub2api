"""Fault injection relay; tc/netem also acts on this container's egress only."""
import gzip
import hashlib
import http.client
import json
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

lock = threading.Lock()
state = {"mode": "normal", "delay": 0, "remaining": 0}
records = []


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def reply(self, code, value):
        raw = json.dumps(value).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/control":
            with lock:
                self.reply(200, {"state": state, "records": records})
        else:
            self.proxy()

    def do_POST(self):
        if self.path == "/control":
            value = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            with lock:
                state.clear()
                state.update(mode="normal", delay=0, remaining=0)
                state.update(value)
            return self.reply(200, {"ok": True})
        try:
            self.proxy()
        except (BrokenPipeError, ConnectionResetError, TimeoutError, OSError) as exc:
            self.close_connection = True
            with lock:
                records.append({"at": time.time(), "kind": "relay_error", "error": type(exc).__name__})

    def proxy(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        sync = self.path == "/internal/sites/sync"
        delivery = self.path == "/internal/sites/events"
        control = sync or delivery
        plain = gzip.decompress(raw) if self.headers.get("Content-Encoding") == "gzip" else raw
        payload = json.loads(plain) if plain else {}
        with lock:
            mode, delay = state.get("mode"), state.get("delay", 0)
            lose = delivery and mode == "lose_ack" and payload.get("events") and state.get("remaining", 0) > 0
            if lose:
                state["remaining"] -= 1
        record = {"at": time.time(), "path": self.path, "kind": "sync" if sync else "delivery" if delivery else "model", "wire_bytes": len(raw), "raw_bytes": len(plain), "gzip": self.headers.get("Content-Encoding") == "gzip", "events": [x["id"] for x in payload.get("events", [])], "mode": mode,
                  "authorization_sha256": hashlib.sha256(self.headers.get("Authorization", "").encode()).hexdigest()}
        if control and mode == "reject":
            record["status"] = 503
            self.record(record)
            return self.reply(503, {"error": "controlled link outage"})
        headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "connection", "transfer-encoding")}
        headers["Host"] = "primary:8080"
        headers["Content-Length"] = str(len(raw))
        if control and mode == "wrong_token":
            headers["Authorization"] = "Bearer deliberately-invalid-lab-token"
        conn = http.client.HTTPConnection("primary", 8080, timeout=40)
        try:
            conn.request(self.command, self.path, raw, headers)
            resp = conn.getresponse()
            record["status"] = resp.status
            if control:
                data = resp.read()
                if resp.status == 200:
                    decoded = gzip.decompress(data) if resp.getheader("Content-Encoding") == "gzip" else data
                    output = json.loads(decoded)
                    record.update(download_wire_bytes=len(data), download_json_bytes=len(decoded), tables=[x["table"] for x in output.get("rows") or []], full=output.get("full"), rows=len(output.get("rows") or []), acknowledged=output.get("acknowledged") or [], version=output.get("version"))
                    if sync and mode == "wrong_identity":
                        output["primary_id"] = "wrong-primary"
                        data = gzip.compress(json.dumps(output).encode())
                    elif sync and mode == "wrong_schema":
                        output["schema"] = "incompatible-schema"
                        data = gzip.compress(json.dumps(output).encode())
                record["lost_ack"] = bool(lose)
                if sync and delay:
                    time.sleep(delay)
                if lose:
                    self.record(record)
                    self.close_connection = True
                    self.connection.shutdown(socket.SHUT_RDWR)
                    return
                self.send_response(resp.status)
                for key, value in resp.getheaders():
                    if key.lower() not in ("content-length", "transfer-encoding", "connection"):
                        self.send_header(key, value)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                self.wfile.flush()
            else:
                self.send_response(resp.status)
                for key, value in resp.getheaders():
                    if key.lower() not in ("content-length", "transfer-encoding", "connection"):
                        self.send_header(key, value)
                self.send_header("Connection", "close")
                self.end_headers()
                self.close_connection = True
                while data := resp.read1(16384):
                    self.wfile.write(data)
                    self.wfile.flush()
            record["elapsed_ms"] = round((time.time() - record["at"]) * 1000, 2)
            self.record(record)
        finally:
            conn.close()

    def record(self, value):
        with lock:
            records.append(value)
            with Path("/evidence/link.jsonl").open("a") as f:
                f.write(json.dumps(value) + "\n")


ThreadingHTTPServer(("0.0.0.0", 8091), Handler).serve_forever()
