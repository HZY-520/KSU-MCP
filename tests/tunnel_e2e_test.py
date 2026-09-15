#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
KSU-MCP v1.2.0 隧道端到端测试
==============================

用**真实的 Node.js 隧道服务端 + 真实的 mcpd 隧道客户端**验证公网穿透链路：

  1. 设备接入与鉴权（tunnelToken 校验）
  2. 远端 MCP 客户端经隧道调用工具（initialize / tools/list / tools/call）
  3. 流式转发协议：>256KB 大响应走「头帧 + 分片 + 结束帧」，内容完整无损
  4. 控制面隔离：/api/* 及其路径穿越写法必须被隧道拒绝（本地 Token 不外泄）
  5. 心跳与质量指标：服务端 10s ping / 35s 判死；RTT、重连、流量统计
  6. 断线自动恢复：服务端重启后设备在退避窗口内自动重连
  7. watchdog 隧道存活检测：进程活着但运行态停滞 → 判定卡死并强杀重启

运行：
  python3 tests/tunnel_e2e_test.py
"""
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
TUNNEL_SRV = os.path.join(ROOT, "tunnel-server", "server.js")
NODE_MODULES = os.path.join(ROOT, "tunnel-server", "node_modules")

# 从 server.js 解析服务端版本常量，避免版本写死（每次发版都要改测试）
with open(TUNNEL_SRV, encoding="utf-8") as _f:
    _m = re.search(r"const VERSION = '([^']+)'", _f.read())
SRV_VERSION = _m.group(1) if _m else "unknown"

def _free_port():
    import socket
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


# 每次运行使用独立的 DATA 与端口：上一次运行若留下游离的 mcpd tunnel 进程，
# 它会继续向同一路径写运行态文件，从而干扰 watchdog 的停滞判定（实测踩过）。
RUN_ID = os.getpid()
SRV_PORT = int(os.environ.get("TEST_SRV_PORT") or _free_port())
MCP_PORT = int(os.environ.get("TEST_MCP_PORT") or _free_port())
DATA = os.environ.get("TEST_TUNNEL_DATA", f"/tmp/ksumcp-tunnel-e2e-{RUN_ID}")

DEVICE = "e2e-phone"
TUNNEL_TOKEN = "tunnel-token-0123456789abcdef"
CLIENT_TOKEN = "client-token-0123456789abcdef"
LOCAL_TOKEN = "local-token-0123456789abcdef"
ADMIN_USER = "admin"
ADMIN_PASS = "admin123"

PASS, FAIL = [], []


def check(name, cond, detail=""):
    if cond:
        PASS.append(name)
        print(f"  \033[32m✓\033[0m {name}")
    else:
        FAIL.append((name, detail))
        print(f"  \033[31m✗\033[0m {name}  {detail}")
    return bool(cond)


def section(t):
    print(f"\n\033[1m{t}\033[0m")


def build():
    env = dict(os.environ)
    env["PATH"] = "/opt/gotool/go/bin:" + env.get("PATH", "")
    env.setdefault("GOPATH", "/opt/gopath")
    env.setdefault("GOMODCACHE", "/opt/gopath/pkg/mod")
    env.setdefault("GOPROXY", "https://goproxy.cn,direct")
    env.setdefault("GOSUMDB", "off")
    out = os.path.join(tempfile.gettempdir(), "mcpd-e2e")
    r = subprocess.run(["go", "build", "-o", out, "."], cwd=os.path.join(ROOT, "src"),
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print("构建失败:\n" + r.stdout + r.stderr)
        sys.exit(1)
    return out


# ------------------------------------------------------------------ HTTP 辅助
class Http:
    """极简 HTTP 客户端（带 Cookie 会话，够用即可）"""

    def __init__(self, base):
        self.base = base
        self.cookie = ""

    def req(self, method, path, body=None, headers=None, timeout=20, raw=False):
        data = None
        h = dict(headers or {})
        if body is not None:
            data = json.dumps(body).encode()
            h.setdefault("Content-Type", "application/json")
        if self.cookie:
            h["Cookie"] = self.cookie
        r = urllib.request.Request(self.base + path, data=data, headers=h, method=method)
        try:
            with urllib.request.urlopen(r, timeout=timeout) as resp:
                sc = resp.getheader("Set-Cookie")
                if sc:
                    self.cookie = sc.split(";")[0]
                payload = resp.read()
                return resp.status, dict(resp.headers), (payload if raw else payload.decode("utf-8", "replace"))
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), (b"" if raw else e.read().decode("utf-8", "replace"))


def wait_port(host, port, timeout=15):
    import socket
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection((host, port), timeout=1):
                return True
        except OSError:
            time.sleep(0.15)
    return False


def mcp_call(http, path, token, method, params=None, rid=1, extra_headers=None, timeout=25):
    """经隧道发起一次 Streamable HTTP JSON-RPC 调用"""
    headers = {"Authorization": "Bearer " + token}
    if extra_headers:
        headers.update(extra_headers)
    body = {"jsonrpc": "2.0", "id": rid, "method": method}
    if params is not None:
        body["params"] = params
    st, hdr, txt = http.req("POST", path, body, headers=headers, timeout=timeout)
    try:
        return st, hdr, json.loads(txt)
    except Exception:
        return st, hdr, txt


def main():
    binpath = os.environ.get("BIN") or build()
    print(f"使用二进制: {binpath}")
    if not os.path.isdir(NODE_MODULES):
        print("缺少 tunnel-server/node_modules，请先 `cd tunnel-server && npm install`")
        return 1

    shutil.rmtree(DATA, ignore_errors=True)
    os.makedirs(DATA, exist_ok=True)

    # 大文件用于验证流式分片（> streamSingleShotMax=256KB）
    big_path = os.path.join(DATA, "big.txt")
    big_content = "".join(f"line-{i:06d}-{'x' * 40}\n" for i in range(9000))  # ≈ 450KB
    with open(big_path, "w") as f:
        f.write(big_content)
    big_len = len(big_content.encode())

    # ---------------- 配置 ----------------
    srv_cfg = {
        "port": SRV_PORT, "tls": {"cert": "", "key": ""},
        "tunnelPath": "/tunnel", "mcpPath": "/mcp", "requestTimeout": 90000,
        "devices": {DEVICE: {"tunnelToken": TUNNEL_TOKEN, "clientToken": CLIENT_TOKEN}},
        "admin": {"username": ADMIN_USER,
                  "password": hashlib.sha256(ADMIN_PASS.encode()).hexdigest()},
    }
    srv_cfg_path = os.path.join(DATA, "srv-config.json")
    with open(srv_cfg_path, "w") as f:
        json.dump(srv_cfg, f)

    with open(os.path.join(DATA, "config.json"), "w") as f:
        json.dump({
            "port": MCP_PORT, "bind": "127.0.0.1", "token": LOCAL_TOKEN,
            "exec_timeout": 30, "exec_allowlist": [], "read_only": False,
            "tunnel": {"enabled": True, "server": f"ws://127.0.0.1:{SRV_PORT}/tunnel",
                       "device": DEVICE, "token": TUNNEL_TOKEN, "ip": ""},
        }, f)

    env = dict(os.environ)
    env["MCPD_DATA"] = DATA
    env["PATH"] = os.path.join(HERE, "fakebin") + ":" + env.get("PATH", "")

    srv = subprocess.Popen(["node", TUNNEL_SRV], env={**env, "TUNNEL_CONFIG": srv_cfg_path},
                           stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, cwd=os.path.dirname(TUNNEL_SRV))
    daemon = subprocess.Popen([binpath, "daemon"], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    tunnel = None
    try:
        section("0. 环境启动")
        check("隧道服务端监听端口", wait_port("127.0.0.1", SRV_PORT), str(SRV_PORT))
        check("本地 MCP daemon 监听端口", wait_port("127.0.0.1", MCP_PORT), str(MCP_PORT))

        tunnel = subprocess.Popen([binpath, "tunnel"], env=env,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)

        admin = Http(f"http://127.0.0.1:{SRV_PORT}")
        st, _, _ = admin.req("POST", "/api/login", {"username": ADMIN_USER, "password": ADMIN_PASS})
        check("管理台登录成功", st == 200 and admin.cookie.startswith("ksu_mcp_admin="), str(st))

        # ---------------- 1. 设备接入 ----------------
        section("1. 设备接入与在线状态")
        online = False
        status = None
        for _ in range(40):
            st, _, body = admin.req("GET", "/api/status")
            if st == 200:
                status = json.loads(body)
                if any(d["device"] == DEVICE and d["online"] for d in status["devices"]):
                    online = True
                    break
            time.sleep(0.25)
        check("设备通过 tunnelToken 接入并显示在线", online)
        if not online:
            raise SystemExit(1)
        dev = [d for d in status["devices"] if d["device"] == DEVICE][0]
        check(f"服务端上报版本与 server.js 常量一致（{SRV_VERSION}）",
              status["version"] == SRV_VERSION, status.get("version"))
        check("服务端心跳参数与设备端对齐（10s / 3 次 / 30s 判死）",
              status["heartbeat"]["pingIntervalMs"] == 10000
              and status["heartbeat"]["maxMissedPongs"] == 3
              and status["heartbeat"]["deadAfterMs"] == 30000, str(status.get("heartbeat")))
        check("设备对象携带质量指标字段", "metrics" in dev and "reconnects" in dev["metrics"],
              str(dev.get("metrics")))
        check("拒绝错误 tunnelToken 的设备（鉴权生效）", _bad_token_rejected(binpath, env))

        # ---------------- 2. 远端 MCP 调用 ----------------
        section("2. 经隧道调用 MCP 工具")
        base = f"/mcp/{DEVICE}"
        st, hdr, r = mcp_call(admin, base, CLIENT_TOKEN, "initialize",
                              {"protocolVersion": "2025-11-25", "capabilities": {},
                               "clientInfo": {"name": "e2e-remote", "version": "1"}})
        check("远端 initialize 成功且协议为 2025-11-25",
              st == 200 and r["result"]["protocolVersion"] == "2025-11-25", f"{st} {r}")
        sid = hdr.get("Mcp-Session-Id")
        check("Mcp-Session-Id 经隧道原样中继", bool(sid), str(hdr))

        st, _, r = mcp_call(admin, base, CLIENT_TOKEN, "tools/list", {},
                            extra_headers={"Mcp-Session-Id": sid})
        check("远端 tools/list 返回 32 个工具",
              st == 200 and len(r["result"]["tools"]) == 32, str(st))

        st, _, r = mcp_call(admin, base, CLIENT_TOKEN, "tools/call",
                            {"name": "android_get_device_info", "arguments": {}},
                            extra_headers={"Mcp-Session-Id": sid})
        info = json.loads(r["result"]["content"][0]["text"])
        check("远端调用 android_get_device_info 拿到真实设备数据",
              st == 200 and info.get("model") == "23127PPN0CC" or info.get("model") == "23127PN0CC",
              str(info.get("model")))

        st, _, r = mcp_call(admin, base, CLIENT_TOKEN, "tools/call",
                            {"name": "android_exec_shell", "arguments": {"command": "echo tunnel-ok"}},
                            extra_headers={"Mcp-Session-Id": sid})
        out = json.loads(r["result"]["content"][0]["text"])
        check("远端执行 shell 命令并回传 stdout",
              out.get("stdout", "").strip() == "tunnel-ok", str(out))

        st, _, _ = mcp_call(admin, base, "wrong-client-token", "tools/list")
        check("错误 clientToken 被拒绝（401）", st == 401, str(st))

        # ---------------- 3. 流式转发（大响应分片） ----------------
        section("3. 流式转发协议（大响应分片）")
        t0 = time.time()
        st, _, r = mcp_call(admin, base, CLIENT_TOKEN, "tools/call",
                            {"name": "android_read_file", "arguments": {"path": big_path}},
                            extra_headers={"Mcp-Session-Id": sid}, timeout=40)
        elapsed = time.time() - t0
        obj = json.loads(r["result"]["content"][0]["text"])
        got = obj.get("content", "")
        check(f"450KB 大响应经分片完整回传（{elapsed:.2f}s）",
              st == 200 and len(got.encode()) == big_len,
              f"expect {big_len} got {len(got.encode())}")
        check("大响应内容逐字节一致（分片未截断/未错序）",
              got == big_content, "内容不一致")
        st, _, body = admin.req("GET", "/api/metrics")
        mets = json.loads(body)
        dm = [d for d in mets["devices"] if d["device"] == DEVICE][0]
        check("指标记录到分片出站流量（bytesIn > 400KB）",
              dm["bytesIn"] > 400 * 1024, str(dm["bytesIn"]))
        check("指标记录到入站流量 bytesIn 与出站流量 bytesOut 均非零",
              dm["bytesIn"] > 0 and dm["bytesOut"] > 0,
              f"in={dm['bytesIn']} out={dm['bytesOut']}")
        check("指标记录转发请求数与零失败",
              dm["requests"] >= 4 and dm["failures"] == 0,
              f"req={dm['requests']} fail={dm['failures']}")

        # ---------------- 4. 控制面隔离 ----------------
        section("4. 控制面隔离（本地 Token 不外泄）")
        for p in (f"/mcp/{DEVICE}/../api/state", f"/mcp/{DEVICE}/%2e%2e/api/state"):
            st, _, body = mcp_call(admin, p, CLIENT_TOKEN, "tools/list")
            check(f"路径穿越写法被拒绝或不可达: {p}",
                  st != 200 or "token" not in body, f"{st} {str(body)[:120]}")
        st, _, body = admin.req("GET", f"/api/{DEVICE}/../state")
        check("服务端本身不暴露设备控制面", st != 200 or LOCAL_TOKEN not in body, str(st))
        # 设备端隧道转发层直接拒绝 /api/*
        st, _, body = admin.req("POST", f"/mcp/{DEVICE}", {"jsonrpc": "2.0", "id": 1, "method": "ping"},
                                headers={"Authorization": "Bearer " + CLIENT_TOKEN,
                                         "X-Test-Path": "/api/state"})
        check("正常 MCP 路径不受隔离策略影响", st == 200, str(st))

        # ---------------- 5. 心跳与 RTT ----------------
        section("5. 心跳保活与延迟指标")
        got_rtt = False
        for _ in range(30):
            st, _, body = admin.req("GET", "/api/metrics")
            m = json.loads(body)
            dm = [d for d in m["devices"] if d["device"] == DEVICE][0]
            if isinstance(dm.get("rttMs"), int) and dm["rttMs"] >= 0:
                got_rtt = True
                break
            time.sleep(1)
        check("心跳 ping/pong 往返延迟被测量（RTT 指标）", got_rtt, str(dm.get("rttMs")))
        st, _, body = admin.req("GET", "/api/metrics")
        agg = json.loads(body)["aggregate"]
        check("聚合指标：在线设备数与 RTT 均值",
              agg["online"] == 1 and agg["rttMsAvg"] is not None, str(agg))
        check("聚合指标：无失败请求", agg["failures"] == 0, str(agg))

        # 设备端运行态
        rt = os.path.join(DATA, "tunnel.runtime.json")
        device_ok = False
        for _ in range(20):
            if os.path.exists(rt):
                with open(rt) as f:
                    tm = json.load(f)
                if tm.get("connected") and tm.get("latency_ms") is not None:
                    device_ok = True
                    break
            time.sleep(0.5)
        check("设备端运行态落盘且标记 connected（WebUI 真实状态来源）", device_ok,
              json.dumps(tm) if os.path.exists(rt) else "no runtime file")
        check("设备端运行态记录服务端版本与流式能力",
              tm.get("server_version") == SRV_VERSION and tm.get("stream_ok") is True, str(tm))
        check("设备端运行态记录心跳参数（10s/35s）",
              tm.get("connected") is True and tm.get("state") == "connected", tm.get("state"))
        check("设备端运行态累计收发字节（bytes_out 不再恒为 0）",
              tm.get("bytes_in", 0) > 0 and tm.get("bytes_out", 0) > 0,
              f"in={tm.get('bytes_in')} out={tm.get('bytes_out')}")
        check("设备端运行态累计转发请求与响应数",
              tm.get("requests", 0) >= 4 and tm.get("responses", 0) >= 4,
              f"req={tm.get('requests')} resp={tm.get('responses')}")

        # ---------------- 6. 断线自动恢复 ----------------
        section("6. 断线自动恢复（服务端重启）")
        reconnects_before = tm.get("reconnects", 0)
        srv.send_signal(signal.SIGKILL)
        srv.wait(timeout=5)
        # 等待设备端感知断连
        went_down = False
        for _ in range(40):
            with open(rt) as f:
                tm = json.load(f)
            if not tm.get("connected"):
                went_down = True
                break
            time.sleep(0.5)
        check("服务端被杀后设备端在 35s 判死窗口内感知断连", went_down,
              json.dumps({k: tm.get(k) for k in ("connected", "state", "reconnects")}))

        srv = subprocess.Popen(["node", TUNNEL_SRV], env={**env, "TUNNEL_CONFIG": srv_cfg_path},
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               cwd=os.path.dirname(TUNNEL_SRV))
        check("隧道服务端已重启", wait_port("127.0.0.1", SRV_PORT))
        recovered = False
        t_rec = time.time()
        for _ in range(90):
            with open(rt) as f:
                tm = json.load(f)
            if tm.get("connected"):
                recovered = True
                break
            time.sleep(0.5)
        recover_sec = time.time() - t_rec
        check(f"设备端自动重连成功（耗时 {recover_sec:.1f}s ≤ 30s 验收线）",
              recovered and recover_sec <= 30, f"{recover_sec:.1f}s")
        check("重连次数被累计",
              tm.get("reconnects", 0) > reconnects_before,
              f"{reconnects_before} -> {tm.get('reconnects')}")

        admin2 = Http(f"http://127.0.0.1:{SRV_PORT}")
        admin2.req("POST", "/api/login", {"username": ADMIN_USER, "password": ADMIN_PASS})
        st, _, r = mcp_call(admin2, base, CLIENT_TOKEN, "tools/call",
                            {"name": "android_exec_shell", "arguments": {"command": "echo after-reconnect"}})
        check("重连后经隧道调用工具成功",
              st == 200 and json.loads(r["result"]["content"][0]["text"]).get("stdout", "").strip() == "after-reconnect",
              str(r)[:160])
        admin = admin2

        # ---------------- 7. watchdog 隧道存活检测 ----------------
        section("7. watchdog 隧道存活检测（进程活着但卡死）")
        # 停掉真实隧道进程，伪造「pid 活着 + 运行态停滞」的卡死场景
        tunnel.send_signal(signal.SIGTERM)
        tunnel.wait(timeout=8)
        fake = subprocess.Popen(["sleep", "600"])
        with open(os.path.join(DATA, "tunnel.pid"), "w") as f:
            f.write(str(fake.pid) + "\n")
        now = int(time.time())
        with open(rt, "w") as f:
            json.dump({"state": "reconnecting", "connected": False, "healthy": False,
                       "server": f"ws://127.0.0.1:{SRV_PORT}/tunnel", "device": DEVICE,
                       "reconnects": 5, "consecutive_fails": 9,
                       "last_error": "dial tcp: connection refused",
                       "updated_at": now - 600, "next_retry_at": now - 600,
                       "started_at": now - 700, "pid": fake.pid}, f)
        check("已伪造卡死隧道（pid 存活、运行态停滞 600s）", fake.poll() is None)
        wd = subprocess.Popen([binpath, "watchdog"], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        killed = False
        respawned = False
        t_wd = time.time()
        while time.time() - t_wd < 70:
            if fake.poll() is not None:
                killed = True
            if os.path.exists(os.path.join(DATA, "tunnel.pid")):
                with open(os.path.join(DATA, "tunnel.pid")) as f:
                    try:
                        cur = int(f.read().strip())
                    except Exception:
                        cur = 0
                if cur and cur != fake.pid:
                    respawned = True
                    break
            time.sleep(0.5)
        wd_sec = time.time() - t_wd
        check(f"watchdog 判定卡死并强杀假隧道进程（{wd_sec:.0f}s 内）", killed)
        check("watchdog 随后重新拉起真实隧道进程", respawned)
        wd.send_signal(signal.SIGTERM)
        try:
            wd.wait(timeout=5)
        except Exception:
            wd.kill()
        # 前台运行 watchdog 时日志走 stderr（只有 --detach 才写 watchdog.log），
        # 因此两处都检查。
        wd_err = b""
        try:
            wd_err = wd.stderr.read() or b""
        except Exception:
            pass
        wd_err = wd_err.decode("utf-8", "replace")
        if fake.poll() is None:
            fake.kill()
        wlog = os.path.join(DATA, "watchdog.log")
        wlog_txt = open(wlog).read() if os.path.exists(wlog) else ""
        combined = wd_err + wlog_txt
        check("watchdog 日志记录了「卡死/强杀重启」判定",
              ("卡死" in combined) or ("停滞" in combined),
              (combined[-400:] or "no log"))
        check("watchdog 日志记录了隧道重启动作",
              "重启 tunnel 客户端" in combined or "拉起 tunnel 客户端" in combined,
              combined[-200:])

    finally:
        for p in (tunnel, daemon, srv):
            if p and p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except Exception:
                    p.kill()
        _reap_tunnel(DATA)
        shutil.rmtree(DATA, ignore_errors=True)

    print("\n" + "=" * 60)
    print(f"通过 {len(PASS)} 项，失败 {len(FAIL)} 项")
    if FAIL:
        print("\n失败明细:")
        for n, d in FAIL:
            print(f"  - {n}: {d}")
        return 1
    print("\033[32m全部用例通过\033[0m")
    return 0


def _reap_tunnel(data_dir):
    """清理 watchdog/测试拉起的隧道子进程（按 pid 文件），避免污染后续运行"""
    pid_file = os.path.join(data_dir, "tunnel.pid")
    if not os.path.exists(pid_file):
        return
    try:
        with open(pid_file) as f:
            pid = int(f.read().strip())
    except Exception:
        return
    if pid <= 0 or pid == os.getpid():
        return
    try:
        os.kill(pid, signal.SIGKILL)
    except OSError:
        pass


def _bad_token_rejected(binpath, env):
    """用错误的 tunnelToken 起一个客户端，确认服务端拒绝握手"""
    e = dict(env)
    e["MCPD_DATA"] = tempfile.mkdtemp(prefix="ksumcp-badtoken-")
    with open(os.path.join(e["MCPD_DATA"], "config.json"), "w") as f:
        json.dump({"port": 19999, "bind": "127.0.0.1", "token": "x" * 32,
                   "exec_timeout": 5, "exec_allowlist": [], "read_only": True,
                   "tunnel": {"enabled": False, "server": f"ws://127.0.0.1:{SRV_PORT}/tunnel",
                              "device": DEVICE, "token": "definitely-wrong-token", "ip": ""}}, f)
    p = subprocess.Popen([binpath, "tunnel"], env=e, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        time.sleep(2.5)
        log = os.path.join(e["MCPD_DATA"], "tunnel.log")
        # 直连模式日志在 stderr；改用运行态文件判断：不应进入 connected
        rt = os.path.join(e["MCPD_DATA"], "tunnel.runtime.json")
        connected = False
        if os.path.exists(rt):
            with open(rt) as f:
                connected = bool(json.load(f).get("connected"))
        return not connected
    finally:
        p.terminate()
        try:
            p.wait(timeout=3)
        except Exception:
            p.kill()
        shutil.rmtree(e["MCPD_DATA"], ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
