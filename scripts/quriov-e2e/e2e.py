"""Quriov 改造回归演练：对一个跑着的（或由本脚本拉起的）new-api 打真实 HTTP，核对 fork 改造的行为。

只连本机的 mock 上游（mock_upstream.py），不连任何真上游、不用任何真 key；root 密码每次随机生成，
只写进 --state 文件（权限 600）。型号名、价格全是占位（本仓公开，不放生产配置）。

用法：
  python3 e2e.py --phase full  --base http://127.0.0.1:3000 --mock-port 18090 --state /tmp/e2e-state.json
  python3 e2e.py --phase smoke --base http://127.0.0.1:3000 --mock-port 18090 --state /tmp/e2e-state.json
  python3 e2e.py --phase full  --bin ./new-api --port 13999 --mock-port 18090 --state ./state.json   # 本地拉起二进制(SQLite)

phase=full  ：建 root → 登录 → 配价 → 建三条渠道(主/备/同步腿) → 建令牌 → 跑全部场景。
phase=smoke ：用 state 里的账号和令牌登录、提交一单、等它完成、再查一次 full 阶段留下的任务。
              给「换镜像（升级 / 回滚）之后老数据还能不能用」这件事用。
退出码：全部检查通过 0，否则 1。

被这个脚本钉住的回归（都是 rc.25 → rc.40 升级时对照演练实测出来的）：
  - 参数覆盖作用于异步任务提交体（改造 3），含 return_error 在发上游之前拒单、不扣费
  - sora 插件接未认领型号时丢字段（aspect_ratio 等）、按视频尺寸枚举拒图片尺寸（改造 4）
  - TASK_PRICE_PATCH 型号被上游内置 token 表达式接管后扣 0（改造 4）
  - 任务失败换渠道重投、只收一次钱、同步腿不进正常路由（改造 1、2）
"""
import argparse
import json
import os
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

QUOTA_PER_UNIT = 500000

# 占位配置。tier_upstream 用的是上游自带内置计费表达式的型号名 —— 改造 4 的计费垫片防的就是它。
CONFIG = {
    "plain_model": "img-std",
    "plain_price": 0.5,
    "tier2k_model": "img-tier-2K",
    "tier4k_model": "img-tier-4K",
    "tier_upstream": "gpt-image-2.5-flare",
    "tier_price": 1.0,
}


def param_override(cfg):
    two_k, four_k = cfg["tier2k_model"], cfg["tier4k_model"]
    mark = {"path": "_e2e_2k", "mode": "full", "value": "1"}
    no_size = [{"path": "size", "mode": "prefix", "value": "", "invert": True, "pass_missing_key": True},
               {"path": "resolution", "mode": "prefix", "value": "", "invert": True, "pass_missing_key": True}]
    return {"operations": [
        {"path": "_e2e_2k", "mode": "set", "value": "1", "logic": "AND",
         "conditions": [{"path": "original_model", "mode": "full", "value": two_k}] + no_size},
        {"path": "size", "mode": "set", "value": "2048x2048", "logic": "AND", "conditions": [mark, {"path": "aspect_ratio", "mode": "full", "value": "1:1"}]},
        {"path": "size", "mode": "set", "value": "2560x1440", "logic": "AND", "conditions": [mark, {"path": "aspect_ratio", "mode": "full", "value": "16:9"}]},
        {"path": "size", "mode": "set", "value": "2048x2048", "logic": "AND",
         "conditions": [mark, {"path": "aspect_ratio", "mode": "prefix", "value": "", "invert": True, "pass_missing_key": True}]},
        {"mode": "return_error", "value": {"message": "e2e: unsupported ratio for 2K", "status_code": 400}, "logic": "AND",
         "conditions": [mark, {"path": "size", "mode": "prefix", "value": "", "invert": True, "pass_missing_key": True}]},
        {"path": "aspect_ratio", "mode": "delete", "logic": "AND", "conditions": [mark]},
        {"path": "_e2e_2k", "mode": "delete", "logic": "AND", "conditions": [mark]},
        {"path": "image_size", "mode": "set", "value": "4K", "keep_origin": True, "logic": "AND",
         "conditions": [{"path": "original_model", "mode": "full", "value": four_k}] + no_size},
    ]}


class Client:
    def __init__(self, base):
        self.base = base
        self.admin = {"New-Api-User": "1"}
        self.cookies = {}

    def req(self, method, path, body=None, headers=None, base=None):
        hdr = {"Content-Type": "application/json"}
        if self.cookies:
            hdr["Cookie"] = "; ".join(f"{k}={v}" for k, v in self.cookies.items())
        hdr.update(headers or {})
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request((base or self.base) + path, data=data, method=method, headers=hdr)
        try:
            with urllib.request.urlopen(request, timeout=60) as resp:
                raw, code, set_cookie = resp.read(), resp.status, resp.headers.get_all("Set-Cookie") or []
        except urllib.error.HTTPError as err:
            raw, code, set_cookie = err.read(), err.code, err.headers.get_all("Set-Cookie") or []
        for item in set_cookie:
            name, _, rest = item.partition("=")
            self.cookies[name.strip()] = rest.split(";", 1)[0]
        try:
            return code, json.loads(raw or b"null")
        except Exception:
            return code, raw.decode(errors="replace")

    def login(self, username, password):
        code, resp = self.req("POST", "/api/user/login", {"username": username, "password": password})
        ok = isinstance(resp, dict) and resp.get("success")
        data = resp.get("data") if isinstance(resp, dict) and isinstance(resp.get("data"), dict) else {}
        # rc.34+ 登录返回短期 access_token（带 Authorization 用）；更早的版本靠 cookie 会话。
        if data.get("access_token"):
            self.admin["Authorization"] = "Bearer " + data["access_token"]
        return ok, code, resp


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--phase", choices=["full", "smoke"], default="full")
    ap.add_argument("--base")
    ap.add_argument("--bin")
    ap.add_argument("--port", type=int, default=13999)
    ap.add_argument("--mock-port", type=int, required=True)
    ap.add_argument("--state", required=True)
    ap.add_argument("--report")
    ap.add_argument("--workdir", help="--bin 模式下的数据目录；多个 phase 共用同一个目录 = 同一个 SQLite 库")
    args = ap.parse_args()

    checks = {}

    def check(name, ok, detail=None):
        checks[name] = {"ok": bool(ok), "detail": detail}
        line = ("PASS " if ok else "FAIL ") + name
        if detail is not None:
            line += "  -> " + json.dumps(detail, ensure_ascii=False)[:600]
        print(line, flush=True)

    cfg = CONFIG
    models = ",".join([cfg["plain_model"], cfg["tier2k_model"], cfg["tier4k_model"]])
    mock = f"http://127.0.0.1:{args.mock_port}"
    proc = None
    if args.bin:
        workdir = args.workdir or tempfile.mkdtemp(prefix="quriov-e2e-")
        os.makedirs(workdir, exist_ok=True)
        # 跨 phase 共用同一对测试密钥（生产也是固定的 CRYPTO_SECRET / SESSION_SECRET）。
        secret_file = os.path.join(workdir, "test-secrets.json")
        if not os.path.exists(secret_file):
            with open(secret_file, "w") as fh:
                json.dump({"session": secrets.token_hex(16), "crypto": secrets.token_hex(16)}, fh)
            os.chmod(secret_file, 0o600)
        test_secrets = json.load(open(secret_file))
        env = dict(os.environ, PORT=str(args.port), SQL_DSN="", SQLITE_PATH=os.path.join(workdir, "e2e.db"),
                   SESSION_SECRET=test_secrets["session"], CRYPTO_SECRET=test_secrets["crypto"], TASK_PRICE_PATCH=models)
        proc = subprocess.Popen([args.bin], cwd=workdir, env=env, stdout=open(os.path.join(workdir, f"server-{args.phase}.log"), "w"), stderr=subprocess.STDOUT)
        base = f"http://127.0.0.1:{args.port}"
        print(f"server log: {workdir}/server-{args.phase}.log", flush=True)
    else:
        base = args.base
    cli = Client(base)

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
        check("startup: /api/status success", isinstance(status, dict) and status.get("success"),
              {"version": (status.get("data") or {}).get("version") if isinstance(status, dict) else None})

        def quota():
            _, resp = cli.req("GET", "/api/user/self", headers=cli.admin)
            data = (resp or {}).get("data") or {} if isinstance(resp, dict) else {}
            return data.get("quota")

        def records():
            return cli.req("GET", "/_records", base=mock)[1]

        def submit(api, model, prompt, **extra):
            body = {"model": model, "prompt": prompt, **extra}
            before_n, before_q = len(records()), quota()
            code, resp = cli.req("POST", "/v1/videos", body, api)
            time.sleep(0.5)
            after_q = quota()
            ups = [r for r in records()[before_n:] if r["kind"] == "submit"]
            tid = (resp or {}).get("id") or (resp or {}).get("task_id") if isinstance(resp, dict) else None
            return {"http": code, "resp": resp, "task": tid, "charged": (before_q - after_q) if None not in (before_q, after_q) else None,
                    "quota_after": after_q, "upstream": ups}

        def wait_done(api, task_id, seconds=150):
            last = None
            deadline = time.time() + seconds
            while time.time() < deadline:
                _, last = cli.req("GET", f"/v1/videos/{task_id}", headers=api)
                if isinstance(last, dict) and last.get("status") in ("completed", "failed"):
                    break
                time.sleep(5)
            return last

        if args.phase == "full":
            password = "E2e-" + secrets.token_urlsafe(18)
            code, resp = cli.req("POST", "/api/setup", {"username": "e2eroot", "password": password, "confirmPassword": password})
            check("setup root account", isinstance(resp, dict) and resp.get("success"), None if isinstance(resp, dict) and resp.get("success") else resp)
            ok, code, resp = cli.login("e2eroot", password)
            check("admin login", ok, None if ok else {"http": code, "resp": resp})

            prices = {cfg["plain_model"]: cfg["plain_price"], cfg["tier2k_model"]: cfg["tier_price"], cfg["tier4k_model"]: cfg["tier_price"]}
            _, resp = cli.req("PUT", "/api/option/", {"key": "ModelPrice", "value": json.dumps(prices)}, cli.admin)
            check("set per-call ModelPrice", isinstance(resp, dict) and resp.get("success"), None if resp.get("success") else resp)

            mapping = {cfg["tier2k_model"]: cfg["tier_upstream"], cfg["tier4k_model"]: cfg["tier_upstream"]}

            def add_channel(name, key, priority, extra=None):
                channel = {"type": 1, "name": name, "key": key, "base_url": mock, "models": models, "group": "default",
                           "priority": priority, "weight": 100, "status": 1, "model_mapping": json.dumps(mapping),
                           "param_override": json.dumps(param_override(cfg))}
                channel.update(extra or {})
                _, resp = cli.req("POST", "/api/channel/", {"mode": "single", "channel": channel}, cli.admin)
                return isinstance(resp, dict) and resp.get("success"), resp

            for name, key, prio, extra in [("e2e-primary", "sk-mockA", 10, None), ("e2e-backup", "sk-mockB", 0, None),
                                           ("e2e-sync-leg", "sk-mockS", 999, {"models": cfg["plain_model"], "setting": json.dumps({"quriov_sync_image_relay": True})})]:
                ok, resp = add_channel(name, key, prio, extra)
                check(f"add channel {name}", ok, None if ok else resp)

            _, resp = cli.req("POST", "/api/token/", {"name": "e2e", "unlimited_quota": True, "expired_time": -1, "remain_quota": 0}, cli.admin)
            check("create api token", isinstance(resp, dict) and resp.get("success"), None if resp.get("success") else resp)
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

            unit = QUOTA_PER_UNIT
            s = submit(api, cfg["tier2k_model"], "a cat", aspect_ratio="16:9")
            up = s["upstream"][0]["body"] if s["upstream"] else {}
            check("param override on async body: 2K 16:9 -> size 2560x1440, upstream model name",
                  s["http"] == 200 and up.get("size") == "2560x1440" and up.get("model") == cfg["tier_upstream"] and "aspect_ratio" not in up and "_e2e_2k" not in up,
                  {"http": s["http"], "upstream_body": up})
            check("billing: 2K charged the per-call ModelPrice (not a built-in token expression)", s["charged"] == int(cfg["tier_price"] * unit), {"charged": s["charged"]})

            s = submit(api, cfg["tier4k_model"], "a dog", aspect_ratio="1:1")
            up = s["upstream"][0]["body"] if s["upstream"] else {}
            check("param override: 4K -> image_size 4K, aspect_ratio kept", s["http"] == 200 and up.get("image_size") == "4K" and up.get("aspect_ratio") == "1:1", {"upstream_body": up})
            check("billing: 4K charged the per-call ModelPrice", s["charged"] == int(cfg["tier_price"] * unit), {"charged": s["charged"]})

            s = submit(api, cfg["tier2k_model"], "bad ratio", aspect_ratio="4:3")
            check("return_error: rejected before upstream, not charged", s["http"] >= 400 and not s["upstream"] and s["charged"] == 0,
                  {"http": s["http"], "resp": s["resp"], "charged": s["charged"]})

            s = submit(api, cfg["plain_model"], "plain", aspect_ratio="9:16", images=["https://ref.mock.invalid/a.png"])
            up = s["upstream"][0] if s["upstream"] else {}
            check("fields pass through; routed to primary; sync leg hidden from normal routing",
                  s["http"] == 200 and up.get("key") == "sk-mockA" and up.get("body", {}).get("aspect_ratio") == "9:16" and up.get("body", {}).get("images") == ["https://ref.mock.invalid/a.png"],
                  {"key": up.get("key"), "body": up.get("body")})
            check("billing: plain model charged the per-call ModelPrice", s["charged"] == int(cfg["plain_price"] * unit), {"charged": s["charged"]})
            ok_task = s["task"]

            s = submit(api, cfg["plain_model"], "square", size="1024x1024")
            up = s["upstream"][0]["body"] if s["upstream"] else {}
            check("image size outside the video size enum is accepted and passed through", s["http"] == 200 and up.get("size") == "1024x1024",
                  {"http": s["http"], "resp": s["resp"] if s["http"] != 200 else None, "upstream_body": up})

            s = submit(api, cfg["plain_model"], "FAIL_ON_A please", aspect_ratio="16:9", images=["https://ref.mock.invalid/b.png"])
            check("resubmit case: first submission went to primary", s["http"] == 200 and s["upstream"] and s["upstream"][0]["key"] == "sk-mockA", {"http": s["http"]})
            final = wait_done(api, s["task"])
            recs = records()
            b_subs = [r for r in recs if r["kind"] == "submit" and r["key"] == "sk-mockB"]
            check("resubmit: upstream failure on primary -> resubmitted to backup", len(b_subs) == 1, {"backup_submits": len(b_subs)})
            if b_subs:
                body = b_subs[0]["body"]
                check("resubmit keeps client fields", body.get("aspect_ratio") == "16:9" and body.get("images") == ["https://ref.mock.invalid/b.png"], {"backup_body": body})
            check("resubmit: same public task id ends completed", isinstance(final, dict) and final.get("status") == "completed", {"final": final})
            check("resubmit: charged exactly once", s["charged"] == int(cfg["plain_price"] * unit) and quota() == s["quota_after"],
                  {"charged_at_submit": s["charged"], "quota_after_submit": s["quota_after"], "quota_now": quota()})
            check("sync leg not used while an async backup exists", not [r for r in recs if r["kind"] == "sync"])

            final = wait_done(api, ok_task, 60)
            check("GET /v1/videos/{id}: completed and keeps upstream result fields", isinstance(final, dict) and final.get("status") == "completed" and final.get("url"),
                  {"keys": sorted(final.keys()) if isinstance(final, dict) else final})

            state = {"password": password, "api_key": key, "task_ids": [ok_task, s["task"]]}
            with open(args.state, "w") as fh:
                json.dump(state, fh)
            os.chmod(args.state, 0o600)
        else:
            state = json.load(open(args.state))
            ok, code, resp = cli.login("e2eroot", state["password"])
            check("smoke: admin login with the account created before the image swap", ok, None if ok else {"http": code, "resp": resp})
            api = {"Authorization": "Bearer " + state["api_key"]}
            for tid in state["task_ids"]:
                code, resp = cli.req("GET", f"/v1/videos/{tid}", headers=api)
                check(f"smoke: task created before the swap is still readable ({tid[:12]}…)", code == 200 and isinstance(resp, dict) and resp.get("status") == "completed", {"http": code, "status": resp.get("status") if isinstance(resp, dict) else resp})
            s = submit(api, cfg["plain_model"], "smoke after swap", aspect_ratio="1:1")
            check("smoke: new submission accepted and charged the per-call price", s["http"] == 200 and s["charged"] == int(cfg["plain_price"] * QUOTA_PER_UNIT),
                  {"http": s["http"], "charged": s["charged"], "resp": s["resp"] if s["http"] != 200 else None})
            final = wait_done(api, s["task"], 90) if s["task"] else None
            check("smoke: new submission polled to completed", isinstance(final, dict) and final.get("status") == "completed", {"final": final})
    finally:
        if proc:
            proc.terminate()
            try:
                proc.wait(10)
            except Exception:
                proc.kill()

    passed = sum(1 for c in checks.values() if c["ok"])
    print(f"== {args.phase}: {passed}/{len(checks)} checks passed", flush=True)
    if args.report:
        with open(args.report, "w") as fh:
            json.dump(checks, fh, ensure_ascii=False, indent=1)
    sys.exit(0 if passed == len(checks) else 1)


if __name__ == "__main__":
    main()
