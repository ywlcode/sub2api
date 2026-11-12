"""Deterministic local upstream. No requests leave the Docker test network."""
import json
import os
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

lock = threading.Lock()
records = []
evidence = Path(os.environ.get("EVIDENCE_DIR", "/evidence"))


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
        if self.path == "/records":
            with lock:
                self.reply(200, records)
        else:
            self.reply(200, {"ok": True})

    def event(self, kind, value, named=True):
        prefix = f"event: {kind}\n" if named else ""
        self.wfile.write((prefix + "data: " + json.dumps(value) + "\n\n").encode())
        self.wfile.flush()

    def do_POST(self):
        try:
            self.handle_model()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def handle_model(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        body = json.loads(raw)
        label = json.dumps(body, ensure_ascii=False)
        scenario = "normal"
        for marker in ("LAB_SLOW", "LAB_STREAM_FAIL", "LAB_MISSING_USAGE", "LAB_HTTP_ERROR"):
            if marker in label:
                scenario = marker
        request_id = self.headers.get("X-Request-ID", "")
        account_key = self.headers.get("Authorization", "") or self.headers.get("x-api-key", "")
        provider = "domestic" if "/domestic/" in self.path else "foreign"
        expected = "domestic-secret" if provider == "domestic" else "foreign-secret"
        record = {"at": time.time(), "path": self.path, "model": body.get("model"), "scenario": scenario,
                  "stream": bool(body.get("stream")), "bytes": len(raw), "request_id": request_id,
                  "reasoning": body.get("reasoning"), "thinking": body.get("thinking"),
                  "account_credential_correct": account_key in (expected, "Bearer " + expected),
                  "call_id_present": "lab_call_1" in label, "summary_present": "lab summary" in label}
        with lock:
            records.append(record)
            with (evidence / "upstream.jsonl").open("a") as f:
                f.write(json.dumps(record) + "\n")
        if not record["account_credential_correct"]:
            return self.reply(401, {"error": {"message": "wrong test upstream credential"}})
        if scenario == "LAB_HTTP_ERROR":
            return self.reply(400, {"error": {"type": "invalid_request_error", "message": "controlled lab rejection"}})
        rid = "resp_" + uuid.uuid4().hex
        text = "lab answer"
        usage = {"input_tokens": 100, "output_tokens": 20, "total_tokens": 120}
        output = [{"id": "msg_" + rid, "type": "message", "role": "assistant", "status": "completed",
                   "content": [{"type": "output_text", "text": text, "annotations": []}]}]
        response = {"id": rid, "object": "response", "model": body["model"], "status": "completed", "output": output, "usage": usage}
        if scenario == "LAB_MISSING_USAGE":
            del response["usage"]
        if not body.get("stream"):
            if scenario == "LAB_SLOW":
                time.sleep(22)
            if self.path.endswith("/chat/completions"):
                response = {"id": rid, "object": "chat.completion", "model": body["model"], "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}}
            elif self.path.endswith("/messages"):
                response = {"id": rid, "type": "message", "role": "assistant", "model": body["model"], "content": [{"type": "text", "text": text}], "stop_reason": "end_turn", "stop_sequence": None, "usage": usage}
            return self.reply(200, response)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        if self.path.endswith("/responses"):
            self.event("response.created", {"type": "response.created", "response": {**response, "status": "in_progress", "output": [], "usage": None}})
            self.event("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": {**output[0], "status": "in_progress", "content": []}})
            self.event("response.content_part.added", {"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": output[0]["id"], "part": {"type": "output_text", "text": "", "annotations": []}})
            self.event("response.output_text.delta", {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": output[0]["id"], "delta": text})
            if scenario == "LAB_SLOW":
                time.sleep(22)
            if scenario == "LAB_STREAM_FAIL":
                time.sleep(2)  # Let the gateway commit/flush its HTTP 200 first.
                response.update(status="failed", error={"code": "server_error", "message": "controlled stream failure"})
                return self.event("response.failed", {"type": "response.failed", "response": response})
            self.event("response.output_text.done", {"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": output[0]["id"], "text": text})
            self.event("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": output[0]})
            self.event("response.completed", {"type": "response.completed", "response": response})
        elif self.path.endswith("/messages"):
            self.event("message_start", {"type": "message_start", "message": {"id": rid, "type": "message", "role": "assistant", "model": body["model"], "content": [], "usage": {"input_tokens": 100, "output_tokens": 0}}})
            self.event("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}})
            self.event("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}})
            self.event("content_block_stop", {"type": "content_block_stop", "index": 0})
            self.event("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 20}})
            self.event("message_stop", {"type": "message_stop"})
        else:
            self.event("chunk", {"id": rid, "object": "chat.completion.chunk", "model": body["model"], "choices": [{"index": 0, "delta": {"role": "assistant", "content": text}, "finish_reason": None}]}, False)
            self.event("chunk", {"id": rid, "object": "chat.completion.chunk", "model": body["model"], "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}}, False)
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()


ThreadingHTTPServer(("0.0.0.0", 8090), Handler).serve_forever()
