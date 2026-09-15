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
# 规范：{service}_{action}_{resource}（service 固定 android）。
# action 是本项目自定的动词（get/list/exec/input/toggle/set/read/write/dump/find/tap/wait/scroll…），
# 因此不写死动词表，只强制「android_ + action + resource 至少三段、全小写」。
ANDROID_NAME_RE = re.compile(r"^android_[a-z][a-z0-9]*_[a-z0-9_]+$")
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

    # 工具数量从二进制动态推导（新增工具时测试无需改动，仅断言自洽）
    _rc, _out = subprocess.run([binpath, "tools", "--json"], env=env,
                               capture_output=True, text=True).returncode, None
    _tj = json.loads(subprocess.run([binpath, "tools", "--json"], env=env,
                                    capture_output=True, text=True).stdout)
    EXP_TOTAL, EXP_CANON, EXP_DEPR = _tj["total"], _tj["canonical"], _tj["deprecated"]
    print(f"二进制自报工具数: total={EXP_TOTAL} canonical={EXP_CANON} deprecated={EXP_DEPR}")

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
    check(f"tools/list 数量与 CLI 自报一致（{EXP_TOTAL} 个，实际 {len(tools)}）", len(tools) == EXP_TOTAL)
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

    # ---------------------------------------------------------- 2.5 屏幕控件树
    section("2.5 屏幕控件树工具（结构化数据替代截图）")
    # 清掉上一步 android_input_text 留下的界面文本状态，保证从干净界面开始
    try:
        os.remove(os.path.join(FAKEBIN, ".last_text"))
    except OSError:
        pass

    o, e = call_tool("android_get_screen_elements")
    check("get_screen_elements 返回屏幕尺寸与元素列表",
          e is None and o["meta"]["screen_width"] == 1080 and o["meta"]["screen_height"] == 2400
          and o["count"] > 0, f"{e} {str(o)[:160]}")
    labels = [el.get("label") for el in o.get("elements", [])]
    check("可点击容器用子孙文本合成 label（微信 版本 8.0.49）",
          any(l and l.startswith("微信") for l in labels), str(labels))
    check("布尔标记仅在 true 时输出（clickable/editable/focused）",
          any(el.get("clickable") for el in o["elements"])
          and any(el.get("editable") for el in o["elements"])
          and any(el.get("focused") for el in o["elements"]))
    check("与祖先同文本的冗余节点被去重（设置 只剩 1 个可点击项）",
          sum(1 for el in o["elements"] if el.get("label") == "设置") == 1,
          str([el.get("label") for el in o["elements"]]))
    check("每个元素都带 center 坐标（可直接点击）",
          all(len(el.get("center", [])) == 2 for el in o["elements"]))
    check("元素带 ref（同屏稳定序号）", all(isinstance(el.get("ref"), int) for el in o["elements"]))

    o, e = call_tool("android_get_screen_elements", {"include_bounds": False})
    check("include_bounds=false 时不返回 bounds（省 token）",
          e is None and all("bounds" not in el for el in o["elements"]))
    o, e = call_tool("android_get_screen_elements", {"filter": "密码"})
    check("filter 命中密码框", e is None and o["count"] == 1 and o["elements"][0].get("editable") is True,
          str(o)[:200])
    o, e = call_tool("android_get_screen_elements", {"max_elements": 2})
    check("max_elements 截断并置 truncated=true",
          e is None and o["count"] == 2 and o["truncated"] is True)

    o, e = call_tool("android_find_element", {"text": "设置"})
    check("find_element 按文本命中可点击容器并给出点击建议",
          e is None and o["total_matches"] == 1 and o["matches"][0].get("clickable") is True
          and "hint" in o, f"{e} {str(o)[:160]}")
    o, e = call_tool("android_find_element", {"editable": True})
    check("find_element 按 editable 筛出 2 个输入框", e is None and o["total_matches"] == 2, str(o)[:120])
    o, e = call_tool("android_find_element", {"password": True})
    check("find_element 定位密码框", e is None and o["total_matches"] == 1
          and o["matches"][0]["id"].endswith("pwd"))
    o, e = call_tool("android_find_element", {"enabled": False})
    check("find_element 定位被禁用控件", e is None and o["total_matches"] == 1
          and o["matches"][0]["label"] == "不可用按钮", str(o)[:160])
    o, e = call_tool("android_find_element", {"checked": True})
    check("find_element 定位已勾选开关", e is None and o["total_matches"] == 1, str(o)[:120])
    o, e = call_tool("android_find_element", {"scrollable": True})
    check("find_element 定位可滚动容器", e is None and o["total_matches"] == 1
          and "RecyclerView" in o["matches"][0]["class"], str(o)[:160])
    o, e = call_tool("android_find_element", {"id_contains": "btn_ok"})
    check("find_element 支持 id_contains", e is None and o["total_matches"] == 1
          and o["matches"][0]["label"] == "确定")
    o, e = call_tool("android_find_element", {"text_contains": "版本"})
    check("find_element 支持 text_contains", e is None and o["total_matches"] >= 1, str(o)[:120])
    o, e = call_tool("android_find_element", {})
    check("find_element 缺选择器时返回 isError", e is not None)
    o, e = call_tool("android_find_element", {"text": "绝对不存在的控件"})
    check("find_element 未命中时给出排查建议",
          e is None and o["total_matches"] == 0 and "hint" in o)

    o, e = call_tool("android_dump_ui_hierarchy", {"format": "json", "max_nodes": 50})
    check("dump_ui_hierarchy(json) 返回嵌套树与节点数",
          e is None and o["returned_nodes"] > 0 and len(o["tree"]) > 0
          and o["meta"]["total_nodes"] >= o["returned_nodes"], str(o)[:160])
    o, e = call_tool("android_dump_ui_hierarchy", {"format": "xml"})
    check("dump_ui_hierarchy(xml) 返回原始控件树 XML",
          e is None and "<hierarchy" in o["xml"] and o["size_bytes"] > 500, str(o)[:120])
    o, e = call_tool("android_dump_ui_hierarchy", {"format": "json", "max_nodes": 3})
    check("dump_ui_hierarchy 遵守 max_nodes 并标记 truncated",
          e is None and o["returned_nodes"] <= 4 and o["truncated"] is True, str(o.get("returned_nodes")))

    open("/tmp/ksumcp-input.log", "w").close()
    o, e = call_tool("android_tap_element", {"text": "设置"})
    check("tap_element 一次调用完成「查找+点击」",
          e is None and o["tapped"] is True and o["element"]["center"] == [540, 570],
          f"{e} {str(o)[:200]}")
    check("tap_element 下发了正确坐标的点击",
          "input tap 540 570" in open("/tmp/ksumcp-input.log").read(),
          open("/tmp/ksumcp-input.log").read()[:120])
    check("tap_element 返回 match_total 与 changed",
          o["match_total"] == 1 and isinstance(o["changed"], bool))
    o, e = call_tool("android_tap_element", {"text": "绝对不存在"})
    check("tap_element 未命中时 tapped=false（不误点）",
          e is None and o["tapped"] is False and "hint" in o)
    o, e = call_tool("android_tap_element", {"text": "确定", "verify": False})
    check("tap_element 可关闭 verify（省一次控件树抓取）",
          e is None and o["tapped"] is True and o["changed"] is False)

    open("/tmp/ksumcp-input.log", "w").close()
    o, e = call_tool("android_set_element_text",
                     {"id": "com.example.demo:id/search", "content": "hello world"})
    check("set_element_text 按 id 定位输入框并写入", e is None and o["filled"] is True, f"{e} {str(o)[:200]}")
    _log = open("/tmp/ksumcp-input.log").read()
    check("set_element_text 执行「聚焦→移到行尾→清空→输入」序列",
          "input tap 540 1915" in _log and "input keyevent 123" in _log
          and "input keyevent 67" in _log and "input text hello world" in _log, _log[:220])
    check("set_element_text 写回校验 verified=true（读回文本等于目标）",
          o.get("verified") is True and o.get("current_text") == "hello world", str(o)[:200])
    o, e = call_tool("android_set_element_text", {"content": "x"})
    check("set_element_text 缺选择器时返回 isError", e is not None)
    o, e = call_tool("android_set_element_text", {"id": "com.example.demo:id/search"})
    check("set_element_text 缺 content 时返回 isError", e is not None)

    o, e = call_tool("android_wait_for_element", {"text": "设置", "timeout_ms": 3000})
    check("wait_for_element 目标已存在时立即满足",
          e is None and o["satisfied"] is True and o["state"] == "present", str(o)[:160])
    o, e = call_tool("android_wait_for_element",
                     {"text": "绝对不存在", "timeout_ms": 700, "interval_ms": 200})
    check("wait_for_element 超时返回 satisfied=false 且记录轮询次数",
          e is None and o["satisfied"] is False and o["polls"] >= 2, str(o)[:160])
    o, e = call_tool("android_wait_for_element",
                     {"text": "绝对不存在", "state": "absent", "timeout_ms": 700})
    check("wait_for_element state=absent 对不存在控件立即满足",
          e is None and o["satisfied"] is True)
    o, e = call_tool("android_wait_for_element", {"ref": 0, "timeout_ms": 500})
    check("wait_for_element 拒绝 ref（会随界面失效）", e is not None)

    o, e = call_tool("android_scroll_to_element", {"text": "设置"})
    check("scroll_to_element 目标已可见时不滚动",
          e is None and o["found"] is True and o["scrolls"] == 0, str(o)[:160])
    o, e = call_tool("android_scroll_to_element",
                     {"text": "绝对不存在", "max_scrolls": 2, "settle_ms": 120})
    check("scroll_to_element 找不到时滚动到上限并返回 found=false",
          e is None and o["found"] is False and o["scrolls"] == 2, str(o)[:160])

    o, e = call_tool("android_get_foreground_app")
    check("get_foreground_app 解析出包名与完整 Activity",
          e is None and o["package"] == "com.example.demo"
          and o["activity"] == "com.example.demo.MainActivity", f"{e} {o}")
    check("get_foreground_app 标注数据来源", o.get("source") == "dumpsys window", str(o))

    # 截图工具保持原样（新增控件树工具不应影响它）
    resp = c.call("tools/call", {"name": "android_screenshot", "arguments": {}})
    _ct = (resp.get("result") or {}).get("content") or []
    check("原有 android_screenshot 仍返回 image 内容块（未被改动）",
          any(b.get("type") == "image" for b in _ct), str([b.get("type") for b in _ct]))
    resp = c.call("tools/call", {"name": "screenshot", "arguments": {}})
    _txt = (resp.get("result") or {}).get("content") or []
    check("弃用别名 screenshot 仍返回 JSON 文本（未被改动）",
          any(b.get("type") == "text" and "data_base64" in b.get("text", "") for b in _txt))

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
              st == 200 and len(json.loads(body)["result"]["tools"]) == EXP_TOTAL, str(st))

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
        check(f"/api/state 报告工具统计（{EXP_CANON} 规范 + {EXP_DEPR} 弃用）",
              stobj["tools"]["canonical"] == EXP_CANON and stobj["tools"]["deprecated"] == EXP_DEPR
              and stobj["tools"]["total"] == EXP_TOTAL,
              str(stobj.get("tools")))
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
              st == 200 and t["total"] == EXP_TOTAL
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

    # ---------------------------------------------------------- 5. CLI 契约
    section("5. CLI 契约（开机脚本依赖项）")

    def cli(args, env=None, timeout=40):
        """执行 mcpd 子命令，返回 (exit_code, stdout)"""
        r = subprocess.run([binpath] + args, env=env or env_base,
                           capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout

    env_base = dict(env)
    cli(["stop"])
    rc, _ = cli(["watchdog-status"])
    check("watchdog-status 未运行时退出码为 2（service.sh / boot-completed.sh 依赖此语义）",
          rc == 2, str(rc))
    rc, out = cli(["watchdog-status"])
    check("watchdog-status 输出可解析的 JSON",
          json.loads(out).get("running") is False)

    rc, out = cli(["start"])
    check("mcpd start 拉起守护并返回 0", rc == 0, f"{rc} {out.strip()[:80]}")
    rc, out = cli(["watchdog-status"])
    check("watchdog-status 运行中退出码为 0", rc == 0, f"{rc} {out.strip()[:80]}")
    check("mcpd start 的 JSON 含 interval_sec 与 tunnel_stale_sec",
          json.loads(out).get("interval_sec") == 10 and json.loads(out).get("tunnel_stale_sec") == 45)

    up = False
    for _ in range(40):
        try:
            s2, _, _ = http("GET", "/health")
            if s2 == 200:
                up = True
                break
        except Exception:
            pass
        time.sleep(0.25)
    check("watchdog 自动拉起 daemon 并通过 /health 探活", up)

    rc, out = cli(["restart"])
    check("mcpd restart（原子重启）返回 0", rc == 0, f"{rc} {out.strip()[:120]}")
    up = False
    for _ in range(40):
        try:
            s2, _, _ = http("GET", "/health")
            if s2 == 200:
                up = True
                break
        except Exception:
            pass
        time.sleep(0.25)
    check("restart 后 daemon 恢复可用", up)

    rc, out = cli(["tools", "--json"])
    tj = json.loads(out)
    check(f"mcpd tools --json 报告 {EXP_TOTAL} 个工具（{EXP_CANON} 规范 + {EXP_DEPR} 弃用）",
          tj["total"] == EXP_TOTAL and tj["canonical"] == EXP_CANON and tj["deprecated"] == EXP_DEPR,
          str(tj)[:120])

    rc, out = cli(["ui-bootstrap"])
    ub = json.loads(out)
    check("mcpd ui-bootstrap 返回 port/token/api_base 供 WebUI 引导",
          rc == 0 and ub["port"] == PORT and ub["has_token"] is True
          and ub["api_base"] == f"http://127.0.0.1:{PORT}", str(ub)[:140])

    rc, out = cli(["ui-state"])
    us = json.loads(out)
    check("mcpd ui-state 为单次聚合调用（含 endpoints/tunnel/tools/watchdog）",
          rc == 0 and all(k in us for k in ("endpoints", "tunnel", "tools", "watchdog", "sessions")))
    check("mcpd ui-state 的隧道段含真实连接态与心跳参数（非仅 pid）",
          us["tunnel"]["heartbeat_sec"] == 10 and us["tunnel"]["read_timeout_sec"] == 35
          and "connected" in us["tunnel"] and "state" in us["tunnel"], str(us["tunnel"])[:140])

    for src in ("mcpd", "watchdog"):
        rc, out = cli(["logs", "5", "--source", src])
        check(f"mcpd logs --source {src} 可用", rc == 0, str(rc))
    rc, _ = cli(["logs", "5", "--source", "tunnel"])
    check("日志文件缺失时 mcpd logs 优雅退出（不崩溃、返回 0）", rc == 0, str(rc))

    rc, _ = cli(["config-set", "--port", "9123"])
    check("config-set 保存并回显配置", rc == 0, str(rc))
    rc, out = cli(["version"])
    check("mcpd version 返回语义化版本", rc == 0 and re.fullmatch(r"\d+\.\d+\.\d+", out.strip()) is not None,
          out.strip())

    rc, _ = cli(["stop"])
    check("mcpd stop 停止全部并返回 0", rc == 0, str(rc))
    rc, _ = cli(["watchdog-status"])
    check("stop 后 watchdog-status 回到退出码 2", rc == 2, str(rc))

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
