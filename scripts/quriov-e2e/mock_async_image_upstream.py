"""Mock「异步出图」上游，给 async_image_plugin_e2e.py 用（只在本机 / CI 跑，不连任何真上游）。

- POST /v1/images/generations/async      记下请求体，回 {"id": ..., "status": "queued"}
                                          prompt 含 REJECT_ASYNC ⇒ 回 400 + 错误对象（提交被拒）
- GET  /v1/images/generations/async/{id} 第一次查回 in_progress，之后回成品对象 {"data":[{"url"}], "size": ...}
                                          prompt 含 FAIL_ASYNC   ⇒ 回 failed + 错误对象
                                          prompt 含 SHRINK_ASYNC ⇒ 成品的 size 报成 1254x1254（上游退档）
- GET  /_records                          到目前为止收到的所有请求（给断言用）
"""
import itertools
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

RECORDS = []
TASKS = {}
LOCK = threading.Lock()
SEQ = itertools.count(1)
SUBMIT = "/v1/images/generations/async"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _json(self, code, obj):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _key(self):
        return (self.headers.get("Authorization") or "").replace("Bearer ", "")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {"_raw": raw.decode(errors="replace")}
        if self.path != SUBMIT:
            with LOCK:
                RECORDS.append({"kind": "unexpected", "path": self.path})
            return self._json(404, {"error": {"message": "not found"}})
        prompt = str(body.get("prompt", ""))
        if "REJECT_ASYNC" in prompt:
            with LOCK:
                RECORDS.append({"kind": "submit_rejected", "key": self._key(), "path": self.path, "body": body})
            return self._json(400, {"error": {"message": "mock upstream refused this prompt", "type": "invalid_request_error", "code": "invalid_request"}})
        tid = f"img_{next(SEQ)}"
        with LOCK:
            RECORDS.append({"kind": "submit", "key": self._key(), "path": self.path, "body": body, "task": tid})
            TASKS[tid] = {"body": body, "polls": 0}
        return self._json(200, {"id": tid, "task_id": tid, "object": "image.generation.task", "model": body.get("model"), "status": "queued", "created_at": 1})

    def do_GET(self):
        if self.path == "/_records":
            with LOCK:
                return self._json(200, RECORDS)
        if not self.path.startswith(SUBMIT + "/"):
            return self._json(404, {"error": {"message": "not found"}})
        tid = self.path[len(SUBMIT) + 1:]
        with LOCK:
            RECORDS.append({"kind": "query", "key": self._key(), "task": tid})
            task = TASKS.get(tid)
            if task:
                task["polls"] += 1
                polls = task["polls"]
        if not task:
            return self._json(404, {"error": {"message": "Task not found", "code": "task_not_found"}})
        prompt = str(task["body"].get("prompt", ""))
        head = {"id": tid, "task_id": tid, "object": "image.generation.task", "model": task["body"].get("model"), "created_at": 1}
        if polls == 1:
            return self._json(200, dict(head, status="in_progress", progress="10%"))
        if "FAIL_ASYNC" in prompt:
            return self._json(200, dict(head, status="failed", progress="10%", error={"message": "Image generation failed (mock)", "code": "generation_failed"}))
        size = "1254x1254" if "SHRINK_ASYNC" in prompt else task["body"].get("size")
        return self._json(200, {"created": 2, "data": [{"url": f"https://cdn.mock.invalid/{tid}.png"}], "size": size,
                                "usage": {"input_tokens": 15, "output_tokens": 196, "total_tokens": 211}})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
