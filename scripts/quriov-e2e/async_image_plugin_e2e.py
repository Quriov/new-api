"""「异步出图」任务插件（plugins/uploads/async-image-relay）的启用 / 回退演练。

对本脚本拉起的 new-api（SQLite）打真实 HTTP：后台上传插件 → 建「任务插件」渠道 → 客户走 /v1/videos。
只连本机的两个 mock 上游，不连任何真上游、不用任何真 key；价格是占位。

  python3 scripts/quriov-e2e/mock_upstream.py 18090 &               # 旧式上游（/v1/videos）
  python3 scripts/quriov-e2e/mock_async_image_upstream.py 18091 &   # 异步出图上游
  go build -o /tmp/new-api . && python3 scripts/quriov-e2e/async_image_plugin_e2e.py --bin /tmp/new-api \
      --legacy-mock-port 18090 --async-mock-port 18091

它核对的是「照启用步骤做，线上会发生什么」：
  - 插件没上传 / 上传但停用时，这些型号照旧走原来的渠道
  - 启用后请求改发到异步出图接口，型号、尺寸、参考图按插件映射；按次价照收，只收一次
  - 不支持的比例、上游提交被拒：不产生任务、不扣费
  - 上游任务失败 / 像素不够档位：任务失败并退款；还有别的渠道时换渠道重投
  - 启用期间，旧式渠道即使优先级更高也不再接这些型号的首次提交（只在换渠道重投时才用得上）
  - 停用插件后回到原来的渠道
退出码：全部检查通过 0，否则 1。
"""
import argparse
import json
import os
import secrets
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from e2e import QUOTA_PER_UNIT, Client  # noqa: E402

PLUGIN_KEY = "async-image-relay"
PLUGIN_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "plugins", "uploads", PLUGIN_KEY, "plugin.js")
MODEL_1K, MODEL_2K = "gpt-image-2.5-1K", "gpt-image-2.5-2K"
PRICES = {MODEL_1K: 0.5, MODEL_2K: 1.0}
CHANNEL_TYPE_OPENAI, CHANNEL_TYPE_TASK_PLUGIN = 1, 61


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", required=True)
    ap.add_argument("--port", type=int, default=13998)
    ap.add_argument("--legacy-mock-port", type=int, required=True)
    ap.add_argument("--async-mock-port", type=int, required=True)
    ap.add_argument("--workdir")
    args = ap.parse_args()

    checks = {}

    def check(name, ok, detail=None):
        checks[name] = bool(ok)
        line = ("PASS " if ok else "FAIL ") + name
        if detail is not None and not ok:
            line += "  -> " + json.dumps(detail, ensure_ascii=False)[:900]
        print(line, flush=True)

    legacy = f"http://127.0.0.1:{args.legacy_mock_port}"
    asyncup = f"http://127.0.0.1:{args.async_mock_port}"
    models = ",".join(PRICES)
    workdir = args.workdir or tempfile.mkdtemp(prefix="quriov-async-image-e2e-")
    os.makedirs(workdir, exist_ok=True)
    env = dict(os.environ, PORT=str(args.port), SQL_DSN="", SQLITE_PATH=os.path.join(workdir, "e2e.db"),
               SESSION_SECRET=secrets.token_hex(16), CRYPTO_SECRET=secrets.token_hex(16), TASK_PRICE_PATCH=models)
    log_path = os.path.join(workdir, "server.log")
    proc = subprocess.Popen([args.bin], cwd=workdir, env=env, stdout=open(log_path, "w"), stderr=subprocess.STDOUT)
    print(f"server log: {log_path}", flush=True)
    cli = Client(f"http://127.0.0.1:{args.port}")

    try:
        status = None
        for _ in range(180):
            try:
                code, status = cli.req("GET", "/api/status")
                if code == 200 and isinstance(status, dict) and status.get("success"):
                    break
            except Exception:
                pass
            time.sleep(1)
        check("startup", isinstance(status, dict) and status.get("success"), status)

        def quota():
            _, resp = cli.req("GET", "/api/user/self", headers=cli.admin)
            return ((resp or {}).get("data") or {}).get("quota") if isinstance(resp, dict) else None

        def records(base):
            return cli.req("GET", "/_records", base=base)[1]

        def submit(api, model, prompt, **extra):
            before = {"legacy": len(records(legacy)), "async": len(records(asyncup)), "quota": quota()}
            code, resp = cli.req("POST", "/v1/videos", {"model": model, "prompt": prompt, **extra}, api)
            time.sleep(0.5)
            tid = (resp.get("id") or resp.get("task_id")) if isinstance(resp, dict) else None
            return {"http": code, "resp": resp, "task": tid, "before": before, "charged": before["quota"] - quota(),
                    "legacy": [r for r in records(legacy)[before["legacy"]:] if r["kind"] == "submit"],
                    "async": [r for r in records(asyncup)[before["async"]:] if r["kind"].startswith("submit")]}

        def error_text(resp):
            return json.dumps(resp, ensure_ascii=False) if not isinstance(resp, str) else resp

        def wait_done(api, task_id, seconds=180):
            last, deadline = None, time.time() + seconds
            while task_id and time.time() < deadline:
                _, last = cli.req("GET", f"/v1/videos/{task_id}", headers=api)
                if isinstance(last, dict) and last.get("status") in ("completed", "failed"):
                    break
                time.sleep(3)
            return last

        password = "E2e-" + secrets.token_urlsafe(18)
        _, resp = cli.req("POST", "/api/setup", {"username": "e2eroot", "password": password, "confirmPassword": password})
        check("setup root account", isinstance(resp, dict) and resp.get("success"), resp)
        ok, code, resp = cli.login("e2eroot", password)
        check("admin login", ok, {"http": code, "resp": resp})
        _, resp = cli.req("PUT", "/api/option/", {"key": "ModelPrice", "value": json.dumps(PRICES)}, cli.admin)
        check("set per-call ModelPrice", isinstance(resp, dict) and resp.get("success"), resp)

        def add_channel(channel):
            channel = dict({"models": models, "group": "default", "weight": 100, "status": 1}, **channel)
            _, resp = cli.req("POST", "/api/channel/", {"mode": "single", "channel": channel}, cli.admin)
            return isinstance(resp, dict) and resp.get("success"), resp

        # 旧式渠道优先级故意设得比插件渠道高：启用插件后首次提交仍然不会落到它上面。
        ok, resp = add_channel({"type": CHANNEL_TYPE_OPENAI, "name": "legacy", "key": "sk-mockA", "base_url": legacy, "priority": 100})
        check("add legacy channel (type 1, priority 100)", ok, resp)

        _, resp = cli.req("POST", "/api/token/", {"name": "e2e", "unlimited_quota": True, "expired_time": -1, "remain_quota": 0}, cli.admin)
        check("create api token", isinstance(resp, dict) and resp.get("success"), resp)
        _, resp = cli.req("GET", "/api/token/?p=1&size=10", headers=cli.admin)
        data = resp.get("data") if isinstance(resp, dict) else None
        items = data.get("items") if isinstance(data, dict) else data
        token = items[0] if items else {}
        key = token.get("key", "")
        if not key or "*" in key:
            _, kresp = cli.req("POST", f"/api/token/{token.get('id')}/key", {}, cli.admin)
            kdata = kresp.get("data") if isinstance(kresp, dict) else None
            key = (kdata.get("key") if isinstance(kdata, dict) else kdata) or ""
        if key and not key.startswith("sk-"):
            key = "sk-" + key
        check("resolve api token key", bool(key) and "*" not in key)
        api = {"Authorization": "Bearer " + key}
        time.sleep(1.5)
        price_1k, price_2k = int(PRICES[MODEL_1K] * QUOTA_PER_UNIT), int(PRICES[MODEL_2K] * QUOTA_PER_UNIT)

        # ── 1. 插件还没上传：走原来的渠道 ──
        s = submit(api, MODEL_1K, "before upload", aspect_ratio="1:1")
        check("before upload: served by the legacy channel", s["http"] == 200 and len(s["legacy"]) == 1 and not s["async"], s)

        # ── 2. 上传但停用：只存源码，路由不变 ──
        source = open(PLUGIN_PATH, encoding="utf-8").read()
        _, resp = cli.req("POST", "/api/plugin/task", {"source": source, "enabled": False, "remark": "e2e"}, cli.admin)
        check("upload plugin with enabled=false", isinstance(resp, dict) and resp.get("success"), resp)
        plugin_channel = {"type": CHANNEL_TYPE_TASK_PLUGIN, "name": "async-image", "key": "sk-mockAsync", "base_url": asyncup, "priority": 0,
                          "setting": json.dumps({"task_plugin_key": PLUGIN_KEY})}
        ok, resp = add_channel(plugin_channel)
        check("a channel cannot be bound to a plugin that is not enabled yet", not ok and "not registered" in error_text(resp), resp)
        time.sleep(1.5)
        s = submit(api, MODEL_1K, "uploaded but disabled", aspect_ratio="1:1")
        check("uploaded but disabled: still served by the legacy channel", s["http"] == 200 and len(s["legacy"]) == 1 and not s["async"], s)

        # ── 3. 启用，紧接着建渠道 ──
        # 顺序只能是「先启用、后建渠道」（上一条检查）。两步之间这些型号没有可用渠道：提交回 503、不扣费。
        # 所以线上这两步要挨着做，挑没有客户调用的时候。
        _, resp = cli.req("POST", f"/api/plugin/task/{PLUGIN_KEY}/status", {"enabled": True}, cli.admin)
        check("enable plugin", isinstance(resp, dict) and resp.get("success"), resp)
        time.sleep(1.5)
        s = submit(api, MODEL_1K, "in the gap", aspect_ratio="1:1")
        check("gap between enabling and adding the channel: 503, nothing sent upstream, not charged",
              s["http"] == 503 and not s["legacy"] and not s["async"] and s["charged"] == 0, s)
        ok, resp = add_channel(plugin_channel)
        check("add task-plugin channel bound to the plugin (priority 0)", ok, resp)
        time.sleep(1.5)

        s = submit(api, MODEL_2K, "a cat", aspect_ratio="16:9", images=["https://ref.mock.invalid/a.png"])
        up = s["async"][0] if s["async"] else {}
        body = up.get("body", {})
        check("enabled: first submission goes to the async image endpoint, not the higher-priority legacy channel",
              s["http"] == 200 and len(s["async"]) == 1 and not s["legacy"] and up.get("path") == "/v1/images/generations/async" and up.get("key") == "sk-mockAsync", s)
        check("enabled: upstream body is mapped (model, pixels, reference images, no client-only fields)",
              body == {"model": "gpt-image-2.5-flare", "prompt": "a cat", "n": 1, "size": "2048x1152", "response_format": "url", "watermark": False,
                       "image": ["https://ref.mock.invalid/a.png"]}, body)
        check("enabled: charged the per-call ModelPrice at submit", s["charged"] == price_2k, {"charged": s["charged"]})
        final = wait_done(api, s["task"])
        check("enabled: polled to completed with the image url; client sees its own model name",
              isinstance(final, dict) and final.get("status") == "completed" and str(final.get("url", "")).endswith(".png")
              and final.get("image_url") == final.get("url") and final.get("model") == MODEL_2K and final.get("size") == "2048x1152" and "usage" not in final, final)
        check("enabled: still charged exactly once after completion", quota() == s["before"]["quota"] - price_2k, {"quota": quota()})

        s = submit(api, MODEL_2K, "bad ratio", aspect_ratio="7:3")
        check("unsupported aspect ratio: rejected before any upstream call, not charged, readable reason",
              s["http"] >= 400 and not s["async"] and not s["legacy"] and s["charged"] == 0 and "aspect_ratio 7:3 is not supported" in error_text(s["resp"]), s)

        s = submit(api, MODEL_1K, "REJECT_ASYNC please", aspect_ratio="1:1")
        check("upstream rejects the submission: no task, not charged, upstream reason passed on",
              s["http"] >= 400 and not s["task"] and s["charged"] == 0 and "mock upstream refused this prompt" in error_text(s["resp"]) and not s["legacy"], s)

        # 两家都失败（旧式 mock 对 sk-mockA + FAIL_ON_A 回 failed）：最终失败并退款。
        s = submit(api, MODEL_1K, "FAIL_ASYNC and FAIL_ON_A", aspect_ratio="1:1")
        final = wait_done(api, s["task"])
        recs = [r for r in records(legacy)[s["before"]["legacy"]:] if r["kind"] == "submit"]
        check("upstream task failed: resubmitted once to the other channel", s["http"] == 200 and s["charged"] == price_1k and len(recs) == 1, {"legacy_submits": len(recs), "s": s})
        check("upstream task failed everywhere: task ends failed and the charge is refunded",
              isinstance(final, dict) and final.get("status") == "failed" and "url" not in final and quota() == s["before"]["quota"], {"final": final, "quota": quota(), "before": s["before"]["quota"]})

        # 只有异步上游失败：换到旧式渠道重投成功，只收一次钱。
        s = submit(api, MODEL_1K, "FAIL_ASYNC only", aspect_ratio="9:16")
        final = wait_done(api, s["task"])
        recs = [r for r in records(legacy)[s["before"]["legacy"]:] if r["kind"] == "submit"]
        check("upstream task failed: resubmit to the legacy channel completes the same task, charged once",
              isinstance(final, dict) and final.get("status") == "completed" and len(recs) == 1 and recs[0]["body"].get("aspect_ratio") == "9:16"
              and quota() == s["before"]["quota"] - price_1k, {"final": final, "legacy_submits": recs})

        # 上游退档（2K 只给了 1254x1254）：判失败，不把小图交给客户；这里两家都失败 ⇒ 退款。
        s = submit(api, MODEL_2K, "SHRINK_ASYNC and FAIL_ON_A", aspect_ratio="1:1")
        final = wait_done(api, s["task"])
        check("delivered size below the tier: task fails, no image url handed out, refunded",
              isinstance(final, dict) and final.get("status") == "failed" and "url" not in final and "data" not in final and quota() == s["before"]["quota"], {"final": final, "quota": quota()})
        _, listing = cli.req("GET", "/api/task/?p=1&page_size=50", headers=cli.admin)
        rows = ((listing or {}).get("data") or {}).get("items") if isinstance(listing, dict) and isinstance(listing.get("data"), dict) else None
        row = next((r for r in rows or [] if r.get("task_id") == s["task"]), None)
        if row is not None:
            check("delivered size below the tier: fail reason is recorded on the task (the last failure wins)", bool(row.get("fail_reason")), row)

        # ── 4. 回退：停用插件（连带停用绑定它的渠道）──
        _, resp = cli.req("POST", f"/api/plugin/task/{PLUGIN_KEY}/status?cascade=true&force=true", {"enabled": False}, cli.admin)
        check("disable plugin (cascade)", isinstance(resp, dict) and resp.get("success"), resp)
        time.sleep(1.5)
        s = submit(api, MODEL_2K, "after rollback", aspect_ratio="1:1")
        check("after disabling: back on the legacy channel, charged the same price", s["http"] == 200 and len(s["legacy"]) == 1 and not s["async"] and s["charged"] == price_2k, s)
    finally:
        proc.terminate()

    failed = [name for name, ok in checks.items() if not ok]
    print(f"\n{len(checks) - len(failed)}/{len(checks)} passed", flush=True)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
