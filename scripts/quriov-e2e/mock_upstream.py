"""Mock upstream for scripts/quriov-e2e/e2e.py (Quriov 改造回归演练用，只在本机/CI 跑，不连任何真上游).

- POST /v1/videos             记下请求体，回 {"id": ..., "status": "queued"}
- GET  /v1/videos/{id}        提交时用的 key 是 sk-mockA 且 prompt 含 FAIL_ON_A ⇒ 回 failed（触发换渠道重投）；
                              其余回 completed，带 image_url / url / metadata.url
- POST /v1/images/generations 同步出图腿：{"data":[{"url": ...}]}
- GET  /_records              到目前为止收到的所有请求（给断言用）
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
        key = self._key()
        if self.path == "/v1/videos":
            tid = f"up_{next(SEQ)}"
            with LOCK:
                RECORDS.append({"kind": "submit", "key": key, "path": self.path, "body": body, "task": tid})
                TASKS[tid] = {"key": key, "body": body}
            return self._json(200, {"id": tid, "object": "video", "status": "queued", "model": body.get("model"), "created_at": 1})
        if self.path == "/v1/images/generations":
            with LOCK:
                RECORDS.append({"kind": "sync", "key": key, "path": self.path, "body": body})
            return self._json(200, {"created": 1, "data": [{"url": "https://cdn.mock.invalid/sync.png"}]})
        return self._json(404, {"error": {"message": "not found"}})

    def do_GET(self):
        if self.path == "/_records":
            with LOCK:
                return self._json(200, RECORDS)
        if self.path.startswith("/v1/videos/"):
            tid = self.path.split("/")[3]
            with LOCK:
                RECORDS.append({"kind": "query", "key": self._key(), "task": tid})
                task = TASKS.get(tid)
            if not task:
                return self._json(404, {"error": {"message": "task not found"}})
            if task["key"] == "sk-mockA" and "FAIL_ON_A" in str(task["body"].get("prompt", "")):
                return self._json(200, {"id": tid, "object": "video", "status": "failed", "progress": 100,
                                        "error": {"message": "upstream glitch 502 (mock)"}})
            url = f"https://cdn.mock.invalid/{tid}.png"
            return self._json(200, {"id": tid, "object": "video", "status": "completed", "progress": 100,
                                    "model": task["body"].get("model"), "image_url": url, "url": url,
                                    "size": task["body"].get("size"), "metadata": {"url": url}})
        return self._json(404, {"error": {"message": "not found"}})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
