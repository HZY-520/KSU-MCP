#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
KSU-MCP v1.2.0 稳定性长跑测试（soak）
=====================================

持续运行真实的「Node 隧道服务端 + mcpd daemon + mcpd 隧道客户端」链路，
周期性采样连接状态与质量指标，并定期发起真实 MCP 调用，用于验证：

  - 连续运行期间无异常断连（reconnects 不增长）
  - 心跳 RTT 稳定、无失败请求
  - 端到端 MCP 调用始终成功

用法：
  python3 tests/soak_test.py [持续时间秒] [采样间隔秒]
默认 7200 秒（2 小时）/ 10 秒采样。

输出（默认写到 /tmp/ksumcp-soak-<pid>/）：
  soak.log    逐次采样记录
  soak.json   汇总结果（供验收引用）
"""
import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRV = os.path.join(ROOT, "tunnel-server", "server.js")
NODE_MODULES = os.path.join(ROOT, "tunnel-server", "node_modules")

DURATION = int(sys.argv[1]) if len(sys.argv) > 1 else 7200
INTERVAL = int(sys.argv[2]) if len(sys.argv) > 2 else 10
DEVICE = "soak-phone"
TT = "soak-tunnel-token-0123456789"
CT = "soak-client-token-0123456789"
LT = "soak-local-token-0123456789"


def free_port():
    import socket
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def build():
    env = dict(os.environ)
    env["PATH"] = "/opt/gotool/go/bin:" + env.get("PATH", "")
    env.setdefault("GOPATH", "/opt/gopath")
    env.setdefault("GOMODCACHE", "/opt/gopath/pkg/mod")
    env.setdefault("GOPROXY", "https://goproxy.cn,direct")
    env.setdefault("GOSUMDB", "off")
    out = os.path.join(os.path.dirname(DATA), "mcpd-soak")
    r = subprocess.run(["go", "build", "-o", out, "."], cwd=os.path.join(ROOT, "src"),
                       env=env, capture_output=True, text=True)
    if r.returncode != 0:
        print(r.stdout + r.stderr)
        sys.exit(1)
    return out


DATA = f"/tmp/ksumcp-soak-{os.getpid()}"
SP, MP = free_port(), free_port()


def http(method, path, body=None, headers=None, timeout=20):
    data = None
    h = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        h.setdefault("Content-Type", "application/json")
    r = urllib.request.Request(f"http://127.0.0.1:{SP}{path}", data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def main():
    if not os.path.isdir(NODE_MODULES):
        print("缺少 tunnel-server/node_modules")
        return 1
    shutil.rmtree(DATA, ignore_errors=True)
    os.makedirs(DATA)
    binpath = build()
    logf = open(os.path.join(DATA, "soak.log"), "w", buffering=1)

    def log(msg):
        line = time.strftime("%H:%M:%S ") + msg
        print(line)
        logf.write(line + "\n")

    with open(f"{DATA}/srv.json", "w") as f:
        json.dump({"port": SP, "tls": {"cert": "", "key": ""}, "tunnelPath": "/tunnel",
                   "mcpPath": "/mcp", "requestTimeout": 90000,
                   "devices": {DEVICE: {"tunnelToken": TT, "clientToken": CT}},
                   "admin": {"username": "admin",
                             "password": hashlib.sha256(b"admin123").hexdigest()}}, f)
    with open(f"{DATA}/config.json", "w") as f:
        json.dump({"port": MP, "bind": "127.0.0.1", "token": LT, "exec_timeout": 30,
                   "exec_allowlist": [], "read_only": False,
                   "tunnel": {"enabled": True, "server": f"ws://127.0.0.1:{SP}/tunnel",
                              "device": DEVICE, "token": TT, "ip": ""}}, f)

    env = dict(os.environ)
    env["MCPD_DATA"] = DATA
    env["PATH"] = os.path.join(HERE, "fakebin") + ":" + env.get("PATH", "")

    procs = []
    srv = subprocess.Popen(["node", SRV], env={**env, "TUNNEL_CONFIG": f"{DATA}/srv.json"},
                           cwd=os.path.dirname(SRV), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    procs.append(srv)
    time.sleep(1)
    daemon = subprocess.Popen([binpath, "daemon"], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    procs.append(daemon)
    time.sleep(0.8)
    tunnel = subprocess.Popen([binpath, "tunnel"], env=env,
                              stdout=open(f"{DATA}/tunnel.log", "w"), stderr=subprocess.STDOUT)
    procs.append(tunnel)

    stats = {
        "duration_requested_s": DURATION, "interval_s": INTERVAL,
        "samples": 0, "online_samples": 0, "offline_samples": 0,
        "mcp_calls": 0, "mcp_failures": 0,
        "rtt_ms_min": None, "rtt_ms_max": None, "rtt_ms_sum": 0, "rtt_count": 0,
        "reconnects_first": None, "reconnects_last": None,
        "started_at": time.time(), "ended_at": None, "elapsed_s": 0,
        "verdict": None, "notes": [],
    }
    exit_code = 1
    try:
        log(f"隧道 2 小时稳定性长跑启动：duration={DURATION}s interval={INTERVAL}s "
            f"srp={SP} mcp={MP} bin={binpath}")
        # 等待首次连接
        rt = f"{DATA}/tunnel.runtime.json"
        for _ in range(60):
            if os.path.exists(rt):
                m = json.load(open(rt))
                if m.get("connected"):
                    break
            time.sleep(0.5)
        else:
            log("设备未能连接，终止")
            return 1

        t0 = time.time()
        next_call = 0.0
        while time.time() - t0 < DURATION:
            time.sleep(INTERVAL)
            stats["samples"] += 1
            try:
                m = json.load(open(rt))
            except Exception as e:
                stats["offline_samples"] += 1
                log(f"运行态读取失败: {e}")
                continue
            conn = bool(m.get("connected"))
            stats["online_samples"] += 1 if conn else 0
            stats["offline_samples"] += 0 if conn else 1
            rtt = m.get("latency_ms")
            if isinstance(rtt, (int, float)):
                stats["rtt_ms_sum"] += rtt
                stats["rtt_count"] += 1
                stats["rtt_ms_min"] = rtt if stats["rtt_ms_min"] is None else min(stats["rtt_ms_min"], rtt)
                stats["rtt_ms_max"] = rtt if stats["rtt_ms_max"] is None else max(stats["rtt_ms_max"], rtt)
            rec = m.get("reconnects", 0)
            if stats["reconnects_first"] is None:
                stats["reconnects_first"] = rec
            stats["reconnects_last"] = rec
            if not conn:
                log(f"[{int(time.time()-t0):5d}s] 断连 state={m.get('state')} "
                    f"reconnects={rec} err={str(m.get('last_error'))[:60]}")
            elif stats["samples"] % 30 == 0:
                log(f"[{int(time.time()-t0):5d}s] 在线 rtt={rtt}ms reconnects={rec} "
                    f"req={m.get('requests')} fails={m.get('consecutive_fails')}")

            # 每 60s 做一次真实端到端 MCP 调用
            if time.time() - t0 >= next_call:
                next_call = time.time() - t0 + 60
                stats["mcp_calls"] += 1
                st, body = http("POST", f"/mcp/{DEVICE}",
                                {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                                 "params": {"name": "android_exec_shell",
                                            "arguments": {"command": "echo soak"}}},
                                headers={"Authorization": "Bearer " + CT})
                ok = False
                try:
                    ok = st == 200 and json.loads(body)["result"]["content"][0]["text"].find("soak") >= 0
                except Exception:
                    ok = False
                if not ok:
                    stats["mcp_failures"] += 1
                    log(f"[{int(time.time()-t0):5d}s] MCP 调用失败 st={st} body={str(body)[:120]}")
        stats["ended_at"] = time.time()
        stats["elapsed_s"] = round(stats["ended_at"] - stats["started_at"], 1)
    finally:
        for p in procs:
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except Exception:
                    p.kill()
        # 记录隧道端日志尾部便于事后分析
        tl = f"{DATA}/tunnel.log"
        if os.path.exists(tl):
            tail = open(tl).read()[-1500:]
        else:
            tail = ""
        stats["tunnel_log_tail"] = tail
        if stats["rtt_count"]:
            stats["rtt_ms_avg"] = round(stats["rtt_ms_sum"] / stats["rtt_count"], 1)
        stats["reconnects_delta"] = (stats["reconnects_last"] or 0) - (stats["reconnects_first"] or 0)
        ok = (stats["offline_samples"] == 0 and stats["mcp_failures"] == 0
              and stats["reconnects_delta"] == 0)
        stats["verdict"] = "PASS" if ok else "FAIL"
        with open(os.path.join(DATA, "soak.json"), "w") as f:
            json.dump(stats, f, indent=2, ensure_ascii=False)
        log(f"结束：采样 {stats['samples']} 次，离线 {stats['offline_samples']} 次，"
            f"MCP 调用 {stats['mcp_calls']} 次失败 {stats['mcp_failures']} 次，"
            f"重连增量 {stats['reconnects_delta']}，RTT 均值 {stats.get('rtt_ms_avg')}ms "
            f"→ {stats['verdict']}")
        log(f"结果文件: {DATA}/soak.json")
        logf.close()
        exit_code = 0 if ok else 1
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
