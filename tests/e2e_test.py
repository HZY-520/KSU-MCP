#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
KSU-MCP v1.2.0 端到端验收测试
================================

覆盖范围：
  1. stdio 传输：initialize 协议协商、tools/list 命名规范校验、全部工具调用
  2. Streamable HTTP 传输：鉴权、会话生命周期、断流 retry 提示、DELETE 回收
  3. 控制 API：/api/state（三网络地址）、/api/logs、/api/tools、CORS 预检
  4. 工具语义正确性：借助 tests/fakebin 下的 Android 命令模拟器，
     校验各 android_* 工具的**解析结果**（而不只是「没报错」）

运行：
  python3 tests/e2e_test.py                 # 自动构建并测试
  BIN=/path/to/mcpd python3 tests/e2e_test.py

退出码：0 = 全部通过；1 = 存在失败用例。
"""
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
FAKEBIN = os.path.join(HERE, "fakebin")
PORT = int(os.environ.get("TEST_PORT", "19123"))
DATA = os.environ.get("TEST_DATA", "/tmp/ksumcp-e2e")
TOKEN = "e2e-test-token-0123456789abcdef"

# 工具命名规范（MCP 2025-11-25）：1-128 字符，[A-Za-z0-9_.-]
TOOL_NAME_RE = re.compile(r"^[A-Za-z0-9_.-]{1,128}$")
# 新增工具的命名规范：{service}_{action}_{resource}
# action 集合按本项目实际语义确定。
ANDROID_NAME_RE = re.compile(r"^android_(get|list|exec|input|toggle|set|read|write)_[a-z0-9_]+$")
# 任务书逐字指定的例外名（仅两段，无 resource）：android_screenshot。
# 需求明确要求该名称，故作为「有据可查的例外」放行，并在 README 中标注。
NAME_EXCEPTIONS = {"android_screenshot"}

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


# ---------------------------------------------------------------- 构建
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


# ---------------------------------------------------------------- stdio 客户端
class StdioClient:
    def __init__(self, binpath, env):
        self.p = subprocess.Popen([binpath], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, env=env, text=True, bufsize=1)
        self.n = 0

    def call(self, method, params=None, notify=False):
        self.n += 1
        msg = {"jsonrpc": "2.0", "method": method}
        if not notify:
            msg["id"] = self.n
        if params is not None:
            msg["params"] = params
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()
        if notify:
            return None
        line = self.p.stdout.readline()
        if not line:
            raise RuntimeError("stdio 连接关闭（进程已退出）")
        return json.loads(line)

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(timeout=5)
        except Exception:
            self.p.kill()


def result_obj(resp):
    """取出 tools/call 的文本内容并尝试解析 JSON"""
    if resp.get("error"):
        return None, resp["error"]
    res = resp.get("result") or {}
    content = res.get("content") or []
    txt = ""
    for c in content:
        if c.get("type") == "text":
            txt += c.get("text", "")
    try:
        return json.loads(txt), None
    except Exception:
        return txt, None


# ---------------------------------------------------------------- HTTP 客户端
def http(method, path, body=None, headers=None, timeout=15):
    url = f"http://127.0.0.1:{PORT}{path}"
    data = None
    h = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        h.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode("utf-8", "replace")


def auth_hdr():
    return {"Authorization": "Bearer " + TOKEN}


# ---------------------------------------------------------------- 主流程
def main():
    binpath = os.environ.get("BIN") or build()
    print(f"使用二进制: {binpath}")

    shutil.rmtree(DATA, ignore_errors=True)
    os.makedirs(DATA, exist_ok=True)
    for f in ("/tmp/ksumcp-input.log", "/tmp/ksumcp-settings.log",
              "/tmp/ksumcp-svc.log", "/tmp/ksumcp-media.log", "/tmp/ksumcp-clip.log"):
        if os.path.exists(f):
            os.remove(f)

    env = dict(os.environ)
    env["MCPD_DATA"] = DATA
    env["PATH"] = FAKEBIN + ":" + env.get("PATH", "")

    # 写配置：固定 Token，便于 HTTP 鉴权测试
    cfg = {
        "port": PORT, "bind": "127.0.0.1", "token": TOKEN,
        "exec_timeout": 30, "exec_allowlist": [], "read_only": False,
        "tunnel": {"enabled": False, "server": "wss://n.huziyang.top/tunnel",
                   "device": "e2e-phone", "token": "", "ip": ""},
    }
    with open(os.path.join(DATA, "config.json"), "w") as f:
        json.dump(cfg, f)

    # ---------------------------------------------------------- 1. stdio
    section("1. stdio 传输")
    c = StdioClient(binpath, env)

    r = c.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                              "clientInfo": {"name": "e2e", "version": "1"}})
    check("initialize 回显受支持的协议版本 2025-06-18",
          r["result"]["protocolVersion"] == "2025-06-18", r["result"]["protocolVersion"])
    check("initialize 返回 serverInfo 与 instructions",
          r["result"]["serverInfo"]["name"] == "ksu-mcpd" and
          "android_get_" in r["result"].get("instructions", ""))
    c.call("notifications/initialized", None, notify=True)

    r = c.call("initialize", {"protocolVersion": "1999-01-01"})
    check("initialize 对未知版本回退到 2025-11-25",
          r["result"]["protocolVersion"] == "2025-11-25", r["result"]["protocolVersion"])

    r = c.call("ping")
    check("ping 返回空结果", r.get("result") == {})

    r = c.call("tools/list")
    tools = r["result"]["tools"]
    names = [t["name"] for t in tools]
    check(f"tools/list 返回 32 个工具（实际 {len(tools)}）", len(tools) == 32)
    check("全部工具名符合 MCP 命名规范字符集/长度",
          all(TOOL_NAME_RE.match(n) for n in names),
          str([n for n in names if not TOOL_NAME_RE.match(n)]))
    check("新增 android_* 工具数 ≥ 5（实际 %d）" %
          len([n for n in names if n.startswith("android_")]),
          len([n for n in names if n.startswith("android_")]) >= 5)
    android_names = [n for n in names if n.startswith("android_")]
    bad_names = [n for n in android_names
                 if not ANDROID_NAME_RE.match(n) and n not in NAME_EXCEPTIONS]
    check("android_* 工具名符合 {service}_{action}_{resource}（仅允许已登记例外 %s）"
          % ",".join(sorted(NAME_EXCEPTIONS)), not bad_names, str(bad_names))
    check("每个工具都有 inputSchema 且 type=object",
          all(isinstance(t.get("inputSchema"), dict) and t["inputSchema"].get("type") == "object"
              for t in tools))
    check("每个工具都有面向 Agent 的描述（≥80 字符）",
          all(len(t.get("description", "")) >= 80 for t in tools),
          str([t["name"] for t in tools if len(t.get("description", "")) < 80]))
    check("弃用别名在描述中标注 deprecated 与替代工具",
          all("已弃用" in t["description"] and "android_" in t["description"]
              for t in tools if t["name"] in
              ("device_info", "exec_command", "screenshot", "getprop")))
    check("键值参数均带 description",
          all("description" in prop
              for t in tools for prop in (t["inputSchema"].get("properties") or {}).values()))

    # ---------------------------------------------------------- 2. 工具语义
    section("2. 工具调用与解析正确性（fakebin 模拟 Android 命令）")

    def call_tool(name, args=None, expect_err=False):
        resp = c.call("tools/call", {"name": name, "arguments": args or {}})
        obj, err = result_obj(resp)
        if err:
            return None, err
        is_err = (resp.get("result") or {}).get("isError", False)
        if expect_err and not is_err:
            return obj, "期望 isError=true 但得到 false"
        if not expect_err and is_err:
            return obj, f"意外错误: {obj}"
        return obj, None

    o, e = call_tool("android_get_device_info")
    check("android_get_device_info 解析品牌/型号/Android 版本",
          e is None and o.get("brand") == "Xiaomi" and o.get("model") == "23127PN0CC"
          and o.get("android") == "14" and o.get("sdk") == "34", f"{e} {o}")
    check("android_get_device_info 检测到 KernelSU 与版本",
          e is None and o.get("root", {}).get("kernelsu") is True
          and "0.9.5" in str(o.get("root", {}).get("kernelsu_version")), f"{o.get('root')}")
    check("android_get_device_info 返回 selinux 状态",
          e is None and o.get("selinux") == "Enforcing", str(o.get("selinux")))

    o, e = call_tool("android_get_battery_status")
    check("android_get_battery_status 解析电量/温度/电压",
          e is None and o.get("level_percent") == 87
          and o.get("temperature_c") == 31.2 and o.get("voltage_v") == 4.231
          and o.get("status") == "Charging", f"{e} {o}")

    o, e = call_tool("android_get_storage_info")
    check("android_get_storage_info 解析分区并过滤伪文件系统",
          e is None and o.get("count", 0) >= 3
          and all(p.get("type") != "tmpfs" for p in o.get("partitions", [])), f"{e} {o}")
    check("android_get_storage_info 返回 key_paths（statfs）",
          e is None and len(o.get("key_paths", [])) > 0
          and "available_bytes" in o["key_paths"][0], f"{o.get('key_paths')}")

    o, e = call_tool("android_get_network_info")
    check("android_get_network_info 解析 WiFi SSID 与开关",
          e is None and o.get("wifi_ssid") == "MyHomeWiFi" and o.get("wifi_enabled") is True,
          f"{e} {o}")
    check("android_get_network_info 返回运营商与网卡类型",
          e is None and o.get("operator") == "CHINA MOBILE"
          and isinstance(o.get("interfaces"), list), f"{e} {o}")

    o, e = call_tool("android_list_packages", {"limit": 2, "offset": 0, "filter": "tencent"})
    check("android_list_packages 支持 filter 与分页",
          e is None and o.get("total") == 2 and o.get("count") == 2
          and o.get("has_more") is False, f"{e} {o}")
    o, e = call_tool("android_list_packages", {"limit": 2, "offset": 0, "state": "all"})
    check("android_list_packages 分页 has_more 正确",
          e is None and o.get("total") == 7 and o.get("count") == 2 and o.get("has_more") is True,
          f"{e} {o}")
    o, e = call_tool("android_list_packages", {"state": "system"})
    check("android_list_packages state=system 过滤生效",
          e is None and o.get("total") == 3, f"{e} {o}")

    o, e = call_tool("android_get_running_processes", {"sort_by": "mem", "limit": 2})
    check("android_get_running_processes 按内存降序",
          e is None and o.get("count") == 2
          and o["processes"][0]["name"] == "com.example.demo"
          and o["processes"][0]["rss"] == 204800, f"{e} {o}")
    o, e = call_tool("android_get_running_processes", {"filter": "mm"})
    check("android_get_running_processes filter 生效",
          e is None and o.get("total") == 1 and o["processes"][0]["pid"] == 1234, f"{e} {o}")

    o, e = call_tool("android_get_logcat", {"filter": "exception", "lines": 100})
    check("android_get_logcat 关键字过滤（大小写不敏感）",
          e is None and o.get("count") == 2, f"{e} {o}")
    o, e = call_tool("android_get_logcat", {"tags": ["AndroidRuntime"], "priority": "E"})
    check("android_get_logcat tags+priority 参数可用", e is None and o.get("count", 0) >= 1, f"{e} {o}")

    # 截屏：断言返回真正的 MCP image 内容块
    resp = c.call("tools/call", {"name": "android_screenshot", "arguments": {}})
    content = (resp.get("result") or {}).get("content") or []
    img = [b for b in content if b.get("type") == "image"]
    txt = [b for b in content if b.get("type") == "text"]
    check("android_screenshot 返回 MCP image 内容块",
          len(img) == 1 and img[0].get("mimeType") == "image/png" and len(img[0].get("data", "")) > 0,
          str([b.get("type") for b in content]))
    check("android_screenshot 附带文本元信息（path/size）",
          len(txt) == 1 and "size_bytes" in json.loads(txt[0]["text"]), str(txt))
    check("android_screenshot 图片为合法 PNG（base64 解码头）",
          len(img) == 1 and __import__("base64").b64decode(img[0]["data"])[:8] == b"\x89PNG\r\n\x1a\n")
    check("android_screenshot 只保留最近 5 张（清理策略）",
          len([f for f in os.listdir(os.path.join(DATA, "tmp")) if f.endswith(".png")]) <= 5)

    o, e = call_tool("android_input_text", {"text": "hello world 你好"})
    check("android_input_text 成功注入并保留空格/中文",
          e is None and open("/tmp/ksumcp-input.log").read().strip() == "input text hello world 你好",
          f"{e} {open('/tmp/ksumcp-input.log').read() if os.path.exists('/tmp/ksumcp-input.log') else 'no log'}")
    o, e = call_tool("android_input_key", {"keycode": "KEYCODE_HOME"})
    check("android_input_key 成功", e is None)
    o, e = call_tool("android_input_tap", {"x": 540, "y": 1200})
    check("android_input_tap 成功", e is None)
    o, e = call_tool("android_input_swipe", {"x1": 540, "y1": 1500, "x2": 540, "y2": 500,
                                             "duration_ms": 300})
    check("android_input_swipe 成功", e is None)
    o, e = call_tool("android_input_text", {})
    check("android_input_text 缺参数返回 isError", e is not None)

    o, e = call_tool("android_get_setting", {"setting": "brightness"})
    check("android_get_setting 读取整数型设置",
          e is None and o.get("value") == 128 and o.get("min") == 0 and o.get("max") == 255,
          f"{e} {o}")
    o, e = call_tool("android_get_setting", {"setting": "wifi"})
    check("android_get_setting 读取布尔型设置（走 cmd wifi status）",
          e is None and o.get("value") is True, f"{e} {o}")
    o, e = call_tool("android_toggle_setting", {"setting": "airplane_mode", "value": True})
    check("android_toggle_setting 飞行模式写入成功并读回校验",
          e is None and o.get("applied") is True and o.get("state", {}).get("value") is True,
          f"{e} {o}")
    o, e = call_tool("android_toggle_setting", {"setting": "volume_music", "int_value": 5})
    check("android_toggle_setting 音量走 media 命令",
          e is None and "media volume --stream 3 --set 5" in o.get("applied_cmd", ""), f"{e} {o}")
    o, e = call_tool("android_toggle_setting", {"setting": "brightness", "int_value": 999})
    check("android_toggle_setting 越界值被拒绝", e is not None, str(e))
    o, e = call_tool("android_get_setting", {"setting": "not_a_setting"})
    check("android_get_setting 非法设置名返回 isError", e is not None)

    o, e = call_tool("android_set_clipboard", {"text": "copied-text"})
    check("android_set_clipboard 成功", e is None and os.path.exists("/tmp/ksumcp-clip.log"))
    o, e = call_tool("android_get_clipboard")
    check("android_get_clipboard 读取文本",
          e is None and o.get("text") == "剪贴板示例内容", f"{e} {o}")

    o, e = call_tool("android_get_prop", {"key": "ro.product.model"})
    check("android_get_prop 单键读取", e is None and o.get("value") == "23127PN0CC", f"{e} {o}")
    o, e = call_tool("android_get_prop")
    check("android_get_prop 无参数返回全部属性",
          e is None and o.get("count", 0) >= 5 and o["props"].get("persist.sys.locale") == "zh-CN",
          f"{e} {o}")

    target = os.path.join(DATA, "rw-test.txt")
    o, e = call_tool("android_write_file", {"path": target, "content": "line1\n"})
    check("android_write_file 写入成功", e is None and os.path.exists(target), f"{e}")
    o, e = call_tool("android_write_file", {"path": target, "content": "line2\n", "append": True})
    check("android_write_file append 生效",
          e is None and open(target).read() == "line1\nline2\n", open(target).read())
    o, e = call_tool("android_read_file", {"path": target})
    check("android_read_file 读回一致",
          e is None and o.get("content") == "line1\nline2\n", f"{e} {o}")
    o, e = call_tool("android_read_file", {"path": DATA})
    check("android_read_file 对目录返回 isError", e is not None)
    o, e = call_tool("android_list_dir", {"path": DATA})
    check("android_list_dir 列出目录",
          e is None and any(x["name"] == "rw-test.txt" for x in o["entries"]), f"{e} {o}")

    o, e = call_tool("android_exec_shell", {"command": "echo hello-ksu"})
    check("android_exec_shell 返回 stdout/exit_code",
          e is None and o.get("stdout", "").strip() == "hello-ksu" and o.get("exit_code") == 0,
          f"{e} {o}")
    o, e = call_tool("android_exec_shell", {"command": "exit 3"})
    check("android_exec_shell 透传退出码",
          e is None and o.get("exit_code") == 3, f"{e} {o}")
    o, e = call_tool("android_exec_shell", {"command": "sleep 5", "timeout": 1})
    check("android_exec_shell 超时被标记",
          e is None and o.get("timed_out") is True and o.get("exit_code") == 124, f"{e} {o}")

    o, e = call_tool("android_unknown_tool")
    check("未知工具返回 isError 且不崩溃", e is not None and "unknown tool" in str(e), str(e))

    # 弃用别名仍需可用
    o, e = call_tool("device_info")
    check("弃用别名 device_info 仍可用",
          e is None and o.get("model") == "23127PN0CC", f"{e}")
    o, e = call_tool("screenshot")
    check("弃用别名 screenshot 仍返回 JSON 文本",
          e is None and "data_base64" in o, f"{e}")

    c.close()

    # ---------------------------------------------------------- 3. HTTP 传输
    section("3. Streamable HTTP 传输")
    daemon = subprocess.Popen([binpath, "daemon"], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        up = False
        for _ in range(50):
            try:
                s, _, b = http("GET", "/health")
                if s == 200:
                    up = True
                    break
            except Exception:
                pass
            time.sleep(0.2)
        check("daemon 启动并通过 /health 探活", up)
        if not up:
            raise SystemExit(1)

        st, _, _ = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 1, "method": "initialize",
                                        "params": {"protocolVersion": "2025-11-25"}})
        check("POST /mcp 未带 Token 返回 401", st == 401, str(st))

        st, hdr, body = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 1, "method": "initialize",
                                             "params": {"protocolVersion": "2025-11-25"}},
                             headers=auth_hdr())
        init = json.loads(body)
        sid = hdr.get("Mcp-Session-Id")
        check("POST /mcp 带 Token 初始化成功并下发 Mcp-Session-Id",
              st == 200 and sid and init["result"]["protocolVersion"] == "2025-11-25", f"{st} {hdr}")

        st, hdr2, body = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 2, "method": "initialize",
                                              "params": {}},
                              headers={**auth_hdr(), "Mcp-Session-Id": sid})
        check("重复 initialize 复用同一会话（修复会话泄漏）",
              hdr2.get("Mcp-Session-Id") == sid, f"{sid} -> {hdr2.get('Mcp-Session-Id')}")

        st, _, body = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 3, "method": "tools/list"},
                           headers={**auth_hdr(), "Mcp-Session-Id": sid})
        check("携带有效会话调用 tools/list 成功",
              st == 200 and len(json.loads(body)["result"]["tools"]) == 32, str(st))

        st, _, body = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 4, "method": "tools/list"},
                           headers={**auth_hdr(), "Mcp-Session-Id": "bogus-session"})
        check("无效会话返回 404 + Session not found",
              st == 404 and "Session not found" in body, f"{st} {body[:80]}")

        st, _, body = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 5, "method": "tools/call",
                                           "params": {"name": "android_get_device_info",
                                                      "arguments": {}}},
                           headers={**auth_hdr(), "Mcp-Session-Id": sid})
        r = json.loads(body)
        check("HTTP 通道调用工具成功",
              st == 200 and r["result"]["content"][0]["type"] == "text"
              and "Xiaomi" in r["result"]["content"][0]["text"], str(st))

        # GET 长流：只读取开头，验证 SSE + retry 提示
        url = f"http://127.0.0.1:{PORT}/mcp"
        req = urllib.request.Request(url, headers={**auth_hdr(), "Mcp-Session-Id": sid})
        with urllib.request.urlopen(req, timeout=10) as r:
            # 用 read1 只取「已有数据」，避免 read(n) 在 chunked 流上阻塞等待凑满 n 字节
            head = r.read1(128).decode("utf-8", "replace")
        check("GET /mcp 返回 SSE 流并下发 retry 断流恢复提示",
              "retry: 3000" in head, repr(head))

        st, _, _ = http("GET", "/mcp", headers=auth_hdr())
        check("GET /mcp 缺少会话返回 404", st == 404, str(st))

        st, _, _ = http("DELETE", "/mcp", headers={**auth_hdr(), "Mcp-Session-Id": sid})
        check("DELETE /mcp 结束会话返回 200", st == 200, str(st))
        st, _, _ = http("POST", "/mcp", {"jsonrpc": "2.0", "id": 6, "method": "tools/list"},
                        headers={**auth_hdr(), "Mcp-Session-Id": sid})
        check("会话结束后再次使用返回 404（会话已回收）", st == 404, str(st))

        # ------------------------------------------------------ 4. 控制 API
        section("4. WebUI 控制 API 与三网络地址")
        st, _, _ = http("GET", "/api/state")
        check("/api/state 未鉴权返回 401", st == 401, str(st))

        st, hdr, body = http("OPTIONS", "/api/state", headers={"Origin": "null"})
        check("OPTIONS 预检返回 204 且带 CORS 头",
              st == 204 and hdr.get("Access-Control-Allow-Origin") == "*"
              and "Authorization" in hdr.get("Access-Control-Allow-Headers", ""),
              f"{st} {hdr.get('Access-Control-Allow-Origin')}")

        st, hdr, body = http("GET", "/api/state", headers=auth_hdr())
        stobj = json.loads(body)
        # 版本断言不写死具体号，避免每次发版都要改测试；只校验形态与端口
        check("/api/state 鉴权通过并返回语义化版本与端口",
              st == 200 and re.fullmatch(r"\d+\.\d+\.\d+", str(stobj.get("version", ""))) is not None
              and stobj["port"] == PORT, f"{st} {stobj.get('version')}")
        check("/api/state 返回 token 供 WebUI 回填（修复 #tunToken 空缺）",
              stobj.get("token") == TOKEN)
        eps = stobj.get("endpoints", {})
        check("三网络地址：local 含 Streamable HTTP 与 SSE 两条",
              len(eps.get("local", [])) == 2
              and {e["transport"] for e in eps["local"]} == {"streamable-http", "sse"},
              str(eps.get("local")))
        check("三网络地址：local URL 正确",
              any(e["url"] == f"http://127.0.0.1:{PORT}/mcp" for e in eps["local"]),
              str([e["url"] for e in eps.get("local", [])]))
        check("三网络地址：LAN 地址可用性标记（当前未开局域网）",
              all(e["available"] is False and e["note"] for e in eps.get("lan", [])),
              str(eps.get("lan")))
        check("三网络地址：public 正确由 wss:// 推导为 https:// 端点",
              len(eps.get("public", [])) == 1
              and eps["public"][0]["url"] == "https://n.huziyang.top/mcp/e2e-phone",
              str(eps.get("public")))
        check("三网络地址：public 不可用时给出原因（隧道未启用）",
              eps["public"][0]["available"] is False and eps["public"][0]["note"],
              str(eps.get("public")))
        check("每个地址都带鉴权方式说明（auth_type/auth_hint）",
              all(e.get("auth_type") and e.get("auth_hint")
                  for grp in ("local", "lan", "public") for e in eps.get(grp, [])))
        check("/api/state 报告工具统计（21 规范 + 11 弃用）",
              stobj["tools"]["canonical"] == 21 and stobj["tools"]["deprecated"] == 11,
              str(stobj.get("tools", {}).get("total")))
        check("/api/state 报告隧道心跳/超时/退避参数",
              stobj["tunnel"]["heartbeat_sec"] == 10 and stobj["tunnel"]["read_timeout_sec"] == 35
              and stobj["tunnel"]["backoff_max_sec"] == 30, str(stobj.get("tunnel")))
        check("/api/state 隧道状态为真实连接态字段（非仅 pid）",
              "connected" in stobj["tunnel"] and "state" in stobj["tunnel"]
              and stobj["tunnel"]["connected"] is False)
        check("/api/state 报告 watchdog 与会话回收配置",
              stobj["watchdog"]["running"] is False
              and stobj["sessions"]["ttl_sec"] == 1800 and stobj["sessions"]["max"] == 512,
              str(stobj.get("sessions")))

        st, _, body = http("GET", "/api/tools", headers=auth_hdr())
        t = json.loads(body)
        check("/api/tools 返回全部工具与参数列表",
              st == 200 and t["total"] == 32
              and any(x["name"] == "android_list_packages"
                      and "offset" in x["params"] for x in t["tools"]))
        check("/api/tools 标注弃用与替代关系",
              any(x["name"] == "screenshot" and x["deprecated"] and x["replaced_by"] == "android_screenshot"
                  for x in t["tools"]))

        for src in ("mcpd", "tunnel", "watchdog"):
            st, _, body = http("GET", f"/api/logs?source={src}&n=50", headers=auth_hdr())
            ok = st == 200 and json.loads(body)["source"] == src
            check(f"/api/logs 支持 source={src}", ok, str(st))

    finally:
        daemon.terminate()
        try:
            daemon.wait(timeout=5)
        except Exception:
            daemon.kill()

    # ---------------------------------------------------------- 汇总
    print("\n" + "=" * 60)
    print(f"通过 {len(PASS)} 项，失败 {len(FAIL)} 项")
    if FAIL:
        print("\n失败明细:")
        for n, d in FAIL:
            print(f"  - {n}: {d}")
        return 1
    print("\033[32m全部用例通过\033[0m")
    return 0


if __name__ == "__main__":
    sys.exit(main())
