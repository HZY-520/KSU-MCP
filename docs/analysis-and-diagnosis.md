# KSU-MCP 现状分析与问题诊断报告

> 基线版本：**v1.1.0**（commit `dea4c11`，tag `v1.1.0`，分支 `main`）
> 目标版本：**v1.2.0**
> 报告范围：`src/main.go`(2501 行)、`module/webroot/index.html`(711 行)、`tunnel-server/{server.js,admin.html}`、
> `module/{customize,service,boot-completed}.sh`、`module/module.prop`、全量文档。
> 所有结论均来自对上述源码的逐行阅读与本地实测，不含推测；推测项已明确标注「待设备验证」。

---

## 一、项目现状

### 1.1 产物与目录结构

| 路径 | 内容 | 规模 |
|---|---|---|
| `src/main.go` | Go 服务端全部实现（无第三方依赖，仅 `gorilla/websocket`） | 2501 行 |
| `module/bin/{arm64,arm}/mcpd` | 预编译静态二进制 | 6.2 MB / 6.4 MB |
| `module/webroot/index.html` | KernelSU Manager 内嵌 WebUI 控制台 | 711 行 |
| `module/{customize,service,boot-completed}.sh` | 安装 / 开机自启 / 开机兜底 | 72+40+32 行 |
| `tunnel-server/server.js` | Node.js 内网穿透服务端 | 616 行 |
| `tunnel-server/admin.html` | 服务端管理台（`/admin`） | 632 行 |
| `ksu-mcp-server-v1.1.0.zip` | 已发布的模块包 | 18 MB（含 `.git`） |

仓库仅 2 个 commit、1 个 tag；`module/README.md` 与根 `README.md` **逐字节相同**（冗余副本）。

### 1.2 已实现的 MCP 工具清单（v1.1.0，共 11 个）

`src/main.go:442-519` 定义，`src/main.go:597-624` 分发。

| # | 工具名 | 参数 | 实现 | 备注 |
|---|---|---|---|---|
| 1 | `device_info` | 无 | `toolDeviceInfo` | 品牌/型号/Android/SDK/ABI/内核/SELinux/uid/root |
| 2 | `exec_command` | `command`(必填), `timeout`, `cwd` | `toolExecCommand` | 受 `read_only`、`exec_allowlist`、超时约束 |
| 3 | `read_file` | `path`(必填), `encoding`, `max_bytes` | `toolReadFile` | utf8 无效时自动 base64，默认上限 4 MB |
| 4 | `write_file` | `path`+`content`(必填), `encoding`, `append` | `toolWriteFile` | `read_only` 下拒绝 |
| 5 | `list_dir` | `path` | `toolListDir` | 名称/类型/大小/权限，按名排序 |
| 6 | `app_list` | `contains` | `toolAppList` | `pm list packages`，**无分页** |
| 7 | `battery_info` | 无 | `toolBatteryInfo` | `/sys/class/power_supply/battery`，缺失回落 `dumpsys battery` |
| 8 | `network_info` | 无 | `toolNetworkInfo` | 网卡/地址/MAC/MTU/主机名/WiFi SSID |
| 9 | `screenshot` | 无 | `toolScreenshot` | `screencap -p`，**base64 塞进 JSON text 文本** |
| 10 | `clipboard_get` | 无 | `toolClipboardGet` | `cmd clipboard get-text` |
| 11 | `getprop` | `key` | `toolGetprop` | 不传 key 返回全部属性 |

**命名规范符合性**：11 个工具**全部**为扁平 snake_case，**无一**符合 `{service}_{action}_{resource}` 三段式（例如应为 `android_get_device_info`）。`inputSchema` 均通过 `schema()/strProp()/intProp()/boolProp()` 构造，结构合法，但描述文本极简（1 行），未面向 AI Agent 说明返回值与约束。

**协议版本**：`protocolVersion = "2025-03-26"`（`src/main.go:43`），未跟随 MCP 最新修订版。

### 1.3 WebUI 组件清单（v1.1.0）

`module/webroot/index.html` — 单文件、无外部依赖、`onclick` 内联绑定。

| 区域 | 元素 | 绑定 | 状态 |
|---|---|---|---|
| 顶栏 | `#watchChip` 守护状态 | `renderWatchChip()`，未运行时可点 → `act('start')` | 可用 |
| 服务状态 | `#pill`/`#pillText`/`#pidVal`/`#portVal`/`#lanChip` | `renderStatus()` | 可用 |
| 服务状态 | 启动/重启/停止/刷新 | `act()` / `refresh()` | **失败后按钮文字永久卡在「处理中」** |
| 接入信息 | 分段标签 本机/局域网/公网 | `selTab()` | **完全失效（`.hidden` 无 CSS 规则）** |
| 接入信息 | `#uLocal`/`#uSse`/复制 | `copyText()` | 可用（本机地址） |
| 接入信息 | `#tab-lan` 局域网列表 | `renderEndpoints()` 每 5s 重建 DOM | 可用但仅 `bind=0.0.0.0` 时展示，无 Token 提示 |
| 接入信息 | `#tab-wan` 公网地址 | `renderEndpoints()` | 可用，但复制按钮在 URL 解析失败时静默无响应 |
| 接入信息 | Token 显示/复制/重新生成 | `toggleToken`/`copyToken`/`regenToken` | 可用（`copyToken` 状态机有缺陷） |
| 运行配置 | 端口 / 命令超时 / 局域网 / 只读 / 开机自启 / 保存 | `saveCfg()`，重载服务 | 可用 |
| 运行配置 | 局域网开关 `onchange` | `lanSwitchChange()` | **仅改提示文字颜色，无实际逻辑** |
| 运行配置 | 开机自启开关 `onchange` | `autoSwitchChange()` | **仅改提示文字，实际生效需点「保存」** |
| 内网穿透 | `#tunPill`/`#tunPillText`/`#tunPub` | `refreshTunnel()` | **仅判 pid，服务假死也显示「已连接」** |
| 内网穿透 | `#tunEnable`/`#tunServer`/`#tunDevice`/`#tunIp` | `renderTunnelCfg()` | `#tunIp` 永不刷新（status 不返回该字段） |
| 内网穿透 | `#tunToken` | **无回填逻辑** | **每次刷新被清空 → 「立即连接/保存配置」必然报「Token 不能为空」** |
| 内网穿透 | 保存/立即连接/断开 | `saveTunnel`/`tunConnect`/`tunStop` | 逻辑存在，被上一条阻断 |
| 运行日志 | `#logs` + 刷新 + 自动刷新 | `loadLogs()` | 可用；每次全量写 200 行 + 强制 `scrollTop` 回流 |
| 全局 | 点击涟漪 `.ripple` | 全局 `click` 委托 | 每次点击创建 DOM + `getBoundingClientRect()` 强制回流 |
| — | **工具列表面板** | — | **完全缺失**（验收标准 3.2 要求的四大面板之一） |
| — | 非 KernelSU 环境降级提示 | — | **缺失**（仅 toast 报错，按钮仍可点、仍失败） |

### 1.4 隧道通信协议流程（v1.1.0）

```
远端 MCP 客户端 ──HTTPS──▶ n.huziyang.top (Node server.js)
                                  │ ①设备侧先主动外连（设备为客户端，免端口映射）
                                  ▼
  设备 mcpd tunnel ──wss──▶ /tunnel?device=<id>&token=<tunnelToken>
                                  │ ②鉴权：devices[device].tunnelToken 全等比较
                                  │ ③服务端下发 hello {version} → 设备用 versionAtLeast(v,"1.1.0") 置 streamOK
   远端请求 ──▶ /mcp/<device>[/sub]
        │ ④服务端校验 Authorization: Bearer <clientToken>（或 ?token=）
        │ ⑤组装 {type:"request", id, method, path, headers, body(base64)} 经 WS 下发（id 单调自增）
        ▼
   设备 handleTunnelRequest ──HTTP──▶ 127.0.0.1:<port><path>（本地注入 Bearer <本地 token>）
        │ ⑥回传（二选一）
        │   单帧：{type:"response", status, headers, body(base64), fin:true}
        │   流式：{type:"response", fin:false} + N×{type:"chunk", body(base64)} + {type:"chunk", done:true}
        ▼
   服务端 writeHead + write + end ──▶ 远端客户端

   保活：设备每 20s WS ping；服务端每 30s ws.ping()；设备读超时 75s；服务端 isAlive 判死
   重连：拨号失败 2s→30s 指数退避（无抖动）；连接断开固定 2s 后重连；
         ifaceSig() 每 15s 比对网卡指纹，变化则主动 Close 触发重连
```

**关键常量**（`src/main.go:57-67`）：`tunnelPingInterval=20s`、`tunnelReadTimeout=75s`、
`tunnelStreamTimeout=30min`、`tunnelChunkSize=64KB`、`streamSingleShotMax=256KB`、
`watchdogInterval=10s`、`watchdogFailMax=3`、服务端心跳周期 `30000ms`。

**传输层实现**：`/mcp` 支持 POST/GET/DELETE（Streamable HTTP，`Mcp-Session-Id` 由设备端维护）；
`/sse` 为传统 SSE（`event: endpoint` 下发 `/mcp?session_id=`，POST 入队 `sess.send` 回推）；
`/health` 免鉴权；`bind` 非回环时强制要求 Token，否则拒绝启动。

---

## 二、六大问题根因诊断

### 问题 1：移动端 WebUI 卡顿 —— 根因 8 项

| # | 位置 | 根因 | 影响 |
|---|---|---|---|
| 1.1 | `index.html:25` | `body{background-attachment:fixed}` + 两层 `radial-gradient` | **滚动每一帧都要重绘整屏渐变**，低端 SoC 掉帧主因 |
| 1.2 | `index.html:32,54` | `backdrop-filter:blur(18px)` 顶栏 + **5 张卡片各 `blur(14px)`** | 每次滚动/重绘触发 6 次离屏模糊合成，GPU 负载极高 |
| 1.3 | `index.html:55,56` | 5 张卡片 `animation:rise .6s ... both` + `animation-delay` 阶梯 | 首屏 5 组动画同时排队；`transform+opacity` 本身可合成，但与 1.2 叠加放大 |
| 1.4 | `index.html:121` | `.sw input:checked+i::after{left:21px}` | 开关滑块用 **`left` 过渡 → 触发布局**，非合成动画 |
| 1.5 | `index.html:48,49,71` | `@keyframes pulse/spinDot` 动画 `box-shadow` | `box-shadow` 关键帧 **逐帧 paint**，无法走合成器；页面同时常驻 2~3 个脉冲元素 |
| 1.6 | `index.html:698-703` | 5s 定时器**每次执行 2~3 次 `ksu.exec`**（`status`+`tunnel-status`，日志开启再加 `logs 200`） | 每次 `ksu.exec` 都要 fork `sh -c` + 启动 mcpd 进程；webview 主线程等待回调，**周期性卡顿** |
| 1.7 | `index.html:388-390,445-502` | `refresh()` 每次都全量重建局域网/公网 DOM（`textContent=''` + `appendChild`） | 每 5s 强制 layout+paint；丢失用户展开状态；与 1.6 叠加成周期性抖动 |
| 1.8 | `index.html:684-692,131-133` | `loadLogs()` 全量写 200 行到 `<pre>`（`white-space:pre-wrap;word-break:break-all`）后设 `scrollTop=scrollHeight` | 长文本断行排版 + 强制 **同步 reflow**，单次可达数十毫秒 |
| 1.9 | `index.html:342-357` | 全局 click 委托：每次点击 `getBoundingClientRect()`（强制 reflow）+ 创建/删除 `.ripple` 节点 | 每次点按引入一次同步布局 |

> 与任务给出的参考实践一致：抽屉/滑块动画应使用 `translate()`；频繁事件需防抖/节流；应减少 WebView 合成层数量。

### 问题 2：UI 虚假/摆设组件 —— 根因 5 项（含 1 个致命缺陷）

| # | 组件 | 根因 | 结论 |
|---|---|---|---|
| 2.1 | **本机 / 局域网 / 公网 分段标签** | `index.html` 的 `<style>` 中**没有任何 `.hidden` 规则**，但 HTML(208,209,234) 与 JS(441,568) 都在用 `classList.toggle('hidden', ...)` | **致命：`selTab()` 是空操作**，三块面板永远同时渲染，「公网」标签点了没反应；`#disabledWarn` 也永远可见。**已本地实测确认** |
| 2.2 | **`#tunToken` 输入框 + 「立即连接」/「保存配置」** | `status` / `tunnel-status` 均**不返回 tunnel Token 明文**，`renderTunnelCfg()` 也不回填；而 `tunConnect()`/`saveTunnel()` 前置校验 `token` 非空 | **页面加载后点这两个按钮必然弹出「请先填写服务器/设备名/Token」**，被用户感知为「按钮是摆设」 |
| 2.3 | **隧道状态胶囊 `#tunPill`** | `refreshTunnel()` 仅依据 `tunnel-status` 的 `running`（= pid 文件存活），**不检测 WS 是否真正连上** | **假状态**：隧道进程卡在重连循环时仍显示「已连接」。同理 watchdog 也只看 pid |
| 2.4 | **`act()` 启动/重启/停止按钮** | `index.html:537-538`：先 `btn.innerHTML='<span class="spin"></span>处理中'`，**之后**才 `const orig = btn.textContent`（取到的是「处理中」）；`finally` 只恢复 `disabled`，不恢复文案 | 任一操作抛错后，按钮文字**永久停留在「处理中」**，且 `orig` 为死变量 |
| 2.5 | **局域网开关 `/ 开机自启开关 的 `onchange`** | `lanSwitchChange()` 只改提示文字颜色；`autoSwitchChange()` 只改提示文案。真正的写入在 `saveCfg()` 里 | 开关拨动**视觉上立即生效、实际未提交**，属误导性反馈 |
| 2.6 | 公网「复制」按钮 | `onclick = () => pub.startsWith('http') && copyText(...)`；URL 解析失败时 `pub='配置无效'`，表达式短路**无任何提示** | 静默失效 |
| 2.7 | 直连 IP `#tunIp` | `renderTunnelCfg()` 中 `$('tunIp').value = $('tunIp').value \|\| tun.ip \|\| ''`，且 `cmdStatus()` 的 tunnel 段**不含 `ip`** | 保存后刷新页面永不回显，用户以为没保存 |
| 2.8 | **工具列表面板** | 未实现 | 验收标准 3.2 要求的四大面板缺一 |
| 2.9 | 非 KernelSU 环境 | `ksuBridge()` 返回 null → 仅 `toast` 报错，5s 后再次报错；所有按钮仍可点且必然失败 | **无优雅降级提示** |

### 问题 3：连接不稳定 —— 根因 7 项

| # | 位置 | 根因 | 量化影响 |
|---|---|---|---|
| 3.1 | `main.go:58,59` | 设备心跳 **20s**、读超时 **75s** | 与任务要求的 10s/35s 不符；NAT 静默丢包后最长 75s 才判死 |
| 3.2 | `server.js:233` | 服务端 `setInterval(...,30000)` + `isAlive` 两拍判定 | **最坏 60s** 才回收死连接；期间设备仍显示「在线」，远端调用打过去只拿到超时 |
| 3.3 | `main.go:1889-1907,1920-1925` | 拨号失败才退避（2s 起翻倍至 30s），**连接断开固定 2s 重连且立即 `backoff=2` 复位**；**无抖动(jitter)** | 网络抖动场景下 2s 固定间隔重连风暴；多设备同时重连产生惊群 |
| 3.4 | `main.go:1866-1887` | `ifaceSig()` **15s 轮询**才识别网络切换 | WiFi↔移动数据恢复最坏 15s + 拨号时间，逼近 30s 验收线；且仅比对网卡地址，路由变化(`/proc/net/route`)不敏感 |
| 3.5 | `main.go:326,343-353` | `httpSessions map[string]bool` **只增不减**：仅 `DELETE /mcp` 才删除，`initialize` 每次都新增；无 TTL、无容量上限 | **长期运行内存泄漏**；2 小时连续运行下会话数持续增长 |
| 3.6 | `main.go:1138-1171` | `/mcp` GET 长流仅发 `: keepalive` 注释，**无断流检测、无恢复机制**；`WriteTimeout=0` 正确但无 `ReadTimeout` | 隧道侧 GET 断流后设备端不感知，需客户端自行重连 |
| 3.7 | `main.go:2034-2061` | `serveTunnel` 的 ping goroutine 与 `handleTunnelRequest` 共用 `wm`；**无 `SetWriteDeadline`** | 若对端停止读取导致 socket 缓冲写满，持锁写阻塞会连带阻塞心跳，最终只能靠读超时兜底 |
| 3.8 | `main.go:1904-1906` | `if backoff < 30 { backoff *= 2 }`，但成功连接后立刻 `backoff = 2` | 连接建立后 3 秒内断开 → 永远以 2s 重连，退避策略实际失效 |

### 问题 4：网络信息排版与三地址展示 —— 根因 5 项

1. `cmdStatus()`（`main.go:1477-1500`）只返回 `port/bind/token/lan_ips/tunnel{enabled,server,device}`，**没有**：公网 URL、`tunnel.ip`、隧道连接状态、传输类型元数据。
2. WebUI **没有统一的「连接信息」卡片**：本机在 `#tab-local`、局域网在 `#tab-lan`、公网在 `#tab-wan`，且因 2.1 缺陷三者同时平铺，层级关系混乱。
3. **局域网地址无 Token 提示**、**无传输类型标注**（Streamable HTTP / SSE 未区分），仅拼 `http://<ip>:<port>/mcp`。
4. `lan_ips` 由 `lanIPs()` 返回**排序后的全部非回环 IPv4**，未区分 WiFi(`wlan0`) 与蜂窝(`rmnet*`)，多网卡时用户无法判断该用哪个。
5. 公网地址是 WebUI 前端用 `new URL(tun.server.replace(/^wss?:\/\//,'https://'))` **现场推导**，硬编码 `/mcp/` 路径（未读取服务端 `mcpPath`），且**在隧道未启用时完全不展示**。

### 问题 5：MCP 工具扩展 —— 根因/缺口

- 现有 11 个工具**命名全部不合规**（缺 `{service}_{action}_{resource}` 三段式），且无弃用标记机制。
- 缺 Android 能力工具：`android_get_storage_info`、`android_get_running_processes`、`android_get_logcat`、`android_input_text`、`android_toggle_setting` 在当前代码中**完全没有对应实现**；`app_list` 无分页、`screenshot` 以 JSON 文本返回 base64（agent 无法直接看图）。
- 工具描述为单行，未说明返回值结构/约束/失败模式；`inputSchema` 缺少 `enum`、`default`、`minimum/maximum` 等约束，AI Agent 选型与填参准确率低。
- `handleToolCall()`（`main.go:404-420`）**只支持 `type:"text"` 内容块**，无法返回 MCP 规范的 `image` 内容类型（截屏类工具的结构性缺陷）。

### 问题 6：公网隧道全场景可达性 —— 根因 6 项

| # | 位置 | 根因 | 后果 |
|---|---|---|---|
| 6.1 | `main.go:1632-1640` | `ensureTunnel()` **只判 `readPidFile(tunnelPidPath())`**，进程活着就直接 return | **进程假死/永久重连失败时 watchdog 永不干预**——「守护了个寂寞」 |
| 6.2 | — | 无任何隧道运行态可观测文件（无 state / 无 lastConnected / 无 reconnectCount / 无 lastError） | WebUI 与 watchdog 都只能看到「pid 存活」，这正是 2.3 的根因 |
| 6.3 | `main.go:1896-1907` | 拨号失败只记日志，不区分「网络不可用」与「鉴权失败」；**鉴权失败（Token 错）也会 2s→30s 无限重试** | 配置错误被掩盖成「网络问题」，无告警 |
| 6.4 | `main.go:1864-1887` | 网卡指纹 15s 轮询 + 无路由/连通性探测；飞行模式恢复后依赖指纹变化，若 IP 未变（DHCP 续租同址）则**不触发重连**，只能等 75s 读超时 | 恢复时延不可控 |
| 6.5 | `server.js` | 服务端**无设备级重连/丢包/延迟指标**，`/api/status` 只给 `online/connectedAt/remote` | 无法在 WebUI 展示「隧道连接质量」（任务 3.6 要求） |
| 6.6 | `server.js:223-233` | 心跳 30s，死连接最坏 60s 才 `terminate()`；期间 `/mcp/<device>` 仍返回「转发中」而非快速失败 | 远端调用悬挂 |

---

## 三、附带发现的其他缺陷（顺带修复）

| # | 位置 | 缺陷 |
|---|---|---|
| A1 | `main.go:928,933` | `toolScreenshot` 每次截图写 `tmp/screen_<ts>.png` **从不清理**，长期占用存储 |
| A2 | `main.go:2097` | `fail()` 手工拼接 JSON 字符串，`errmsg` 未做 JSON 转义（当前均为常量，隐患） |
| A3 | `main.go:839-846` | `toolAppList` 对小写过滤敏感但 `pm` 输出为大写无关；未提供分页，几百个包一次性回传 |
| A4 | `main.go:871-885` | `battery_info` 字段值混用 string/int，`temp_celsius`/`voltage_V` 为空字符串而非 null，agent 解析易错 |
| A5 | `main.go:1094-1098` | 携带有效 `Mcp-Session-Id` 再次 `initialize` 时**无条件签发新会话**，旧会话泄漏（与 3.5 叠加） |
| A6 | `module/webroot/index.html:536-559` | `act('restart')` 用 `; sleep 1; ` 串命令，无错误传播；`stop` 后立即 `refresh` 可能读到未清理的 pid |
| A7 | `customize.sh:63` / `service.sh:38` / `boot-completed.sh:28` | 三处用 `grep -q '"running": true'` 解析 JSON，格式一改即失效（脆弱耦合） |
| A8 | `module/module.prop` | `version=v1.1.0`/`versionCode=2` 与 `README`/`BUILD.md` 中多处硬编码版本号需同步 |
| A9 | `README.md:236` | 文档写「客户端 20s WS 心跳 + 75s 读超时」，与新实现（10s/35s）不一致，需同步更新 |

---

## 四、修复策略（对应阶段三）

| 目标 | 手段 |
|---|---|
| WebUI 卡顿 | ①去掉 `background-attachment:fixed`，改独立固定合成层；②卡片去 `backdrop-filter`，仅顶栏保留；③脉冲动画改 `transform/opacity`；④开关滑块改 `translateX`；⑤轮询降频+失焦暂停+失败退避+**载荷变更才重绘**；⑥LAN/WAN 列表改**增量 diff**，不重建 DOM；⑦日志改**节流+变更比对+行数上限+固定高度容器**；⑧移除 JS 涟漪，改纯 CSS 按压反馈 |
| UI 虚假组件 | ①补 `.hidden{display:none!important}`；②新增 `mcpd ui-state` / `GET /api/state` **一次性返回含 tunnel token 的全量状态**，彻底解决 `#tunToken` 回填；③隧道状态改**真实连接态**（state 文件）；④修复 `act()` 按钮文案恢复；⑤开关改为「即时写入」语义并明确「保存」按钮职责；⑥补齐**工具列表面板**；⑦非 KernelSU 环境显示**降级横幅**并禁用写操作按钮 |
| 连接不稳定 | 心跳 10s / 读超时 35s（两端一致）；退避改 `2s→30s` 指数 + **±20% 抖动**，断线也走退避；网卡指纹改 **5s** 轮询并加入默认路由与 DNS 变化；`httpSessions` 加 **TTL + 上限 + 定期回收**；GET 长流加断流检测与 `WriteDeadline` |
| 三地址 | `status` 输出新增 `endpoints{local,lan[],public}`（含 transport/url/auth/示例）；WebUI 统一「连接信息」卡片，三地址各带传输类型 + `Bearer <Token>` 提示 + 一键复制；局域网地址标注网卡类型 |
| 工具扩展 | 新增 10 个 `android_*` 工具（三段式命名、完整 `inputSchema`、面向 Agent 的详细 docstring）；`handleToolCall` 支持 `image` 内容块；旧 11 名保留并标记 `deprecated` 别名；协议版本协商至 **2025-11-25** |
| 隧道全场景 | 新增 `tunnel.runtime.json` 运行态（state/lastConnectedAt/reconnectCount/latencyMs/lastError/consecutiveFails）；watchdog `ensureTunnel` 改**存活+新鲜度**双判定，卡死则强制重启进程；服务端 `/api/status` 增加设备级指标（reconnects/lastSeen/rttMs/bytesIn/bytesOut）；心跳 10s、35s 判死并快速失败 |

---

## 五、验证与限制说明

**本地可验证**：Go 编译（linux/amd64 实跑）、stdio 与 Streamable HTTP 全工具调用、`/api/state` 契约、
隧道端到端（本地起 Node 服务端 + 真实 mcpd 隧道客户端，含流式分帧/大响应/断线重连/指标）、
WebUI 用 jsdom 驱动冒烟测试（注册全部 onclick、逐一触发并断言无异常且发出预期命令）。

**需真机验证（本次无法覆盖，将如实标注）**：
1. KernelSU Manager 内嵌 WebView 的实际滚动帧率与 `fetch` 跨源可用性（`file://` → `http://127.0.0.1`）；
2. arm64/arm 二进制在 Android 上的静态可执行性与 SELinux 策略通过性；
3. 真实 WiFi↔4G 切换、飞行模式恢复、>5 分钟持续断网的恢复时延；
4. `screencap`/`input`/`svc`/`logcat` 在各厂商 ROM 上的可用性差异（沿用 v1.1.0 的「已知限制」口径）。

WebUI 已按「**优先 `fetch` 本地 API，失败自动回退 `ksu.exec`**」设计：即使 WebView 禁止跨源请求，
功能仍完整可用（仅性能回退），不会出现组件失效。

---

## 六、修复对照表与验证证据（v1.2.0 实施结果）

### 6.1 六大问题的修复落点

| 问题 | 修复落点 | 验证用例 |
|---|---|---|
| 1 卡顿（1.1~1.9） | `webroot/index.html`：去 `background-attachment:fixed`（改独立固定合成层 `.bg`）、去卡片 `backdrop-filter`、脉冲动画改 `transform+opacity`、开关改 `translateX`、移除 JS 涟漪、列表 keyed-diff、日志限 300 行 + 变更比对 + `contain:content` 固定高度、轮询 6s 自适应 + `visibilitychange` 暂停 + 失败指数退避 | `tests/webui_test.js` F 组 11 项静态约定断言 |
| 2 虚假组件（2.1~2.9） | 补 `.hidden{display:none!important}`；新增 `/api/state` 与 `mcpd ui-state` 单次聚合；`btnBusy()` 统一恢复按钮文案；隧道状态改读 `tunnel.runtime.json` 真实连接态；Token 改为「留空沿用已保存值」；补齐工具列表面板；降级横幅 + `lockWrites()` 禁用写操作 | `tests/webui_test.js` A/C/E 组；`tests/e2e_test.py` 控制 API 组 |
| 3 连接不稳定（3.1~3.8） | 心跳 10s / 判死 35s；退避 2s→30s + ±20% 抖动且「存活 <30s 不重置」；网卡指纹 5s 且纳入默认路由；会话 TTL 30min + 上限 512 + 每分钟回收；GET 长流下发 SSE `retry` + `SetWriteDeadline` | `src/main_test.go` 时序常量与抖动边界；`tests/tunnel_e2e_test.py` 断线恢复组；`tests/soak_test.py` |
| 4 三地址（4.1~4.5） | `buildEndpoints()` 统一产出 local/lan/public，含 transport、auth_type、auth_hint、可用性与原因；`/api/state` 与 `mcpd status` 共用；WebUI「连接信息」卡片 keyed-diff 渲染 + 一键复制 | `src/main_test.go` `TestBuildEndpoints`；`tests/e2e_test.py` 三网络地址组 |
| 5 工具扩展（5） | 21 个 `android_{action}_{resource}` 规范工具（含任务书要求的 10 个）；`handleToolCall` 支持 `image` 内容块；旧 11 名标记弃用并指向替代工具 | `src/main_test.go` `TestToolRegistryIntegrity`；`tests/e2e_test.py` 全部工具解析断言（含 PNG magic 校验） |
| 6 隧道全场景（6.1~6.6） | watchdog「存活 + 运行态新鲜度」双判定（`NextRetryAt` 区分合法退避）；隧道运行态落盘；服务端心跳 10s / 3 次丢包判死并暴露设备级指标 | `tests/tunnel_e2e_test.py` 心跳指标组与 watchdog 卡死组 |

### 6.2 附带发现（A1~A9）的处理

| # | 处理结果 |
|---|---|
| A1 截屏文件无限增长 | 已修复：`pruneScreenshots()` 仅保留最近 5 张（`screenshotKeep`），e2e 有断言 |
| A2 `fail()` 手工拼 JSON 未转义 | 已修复：改用 `json.Marshal` 构造错误体 |
| A3 `app_list` 无分页 | 已修复：规范工具 `android_list_packages` 支持 `filter`/`state`/`offset`/`limit`/`sort` 与 `has_more` |
| A4 电池字段类型混杂 | 已修复：统一为数值或 `null`，新增 `source` 标注数据来源（sysfs / dumpsys） |
| A5 重复 initialize 泄漏会话 | 已修复：携带有效 `Mcp-Session-Id` 时复用而不新签，并对 map 加 TTL + 容量上限 |
| A6 `act('restart')` 拼 `sleep` | 已修复：新增原子 `mcpd restart`（进程内 stop → start，等待 watchdog pid 就绪） |
| A7 shell 用 `grep` 解析 JSON | 已修复：`watchdog-status` 以退出码表达结论（0=运行中 / 2=未运行），三个脚本同步改写 |
| A8 版本号多处硬编码 | 已修复：`appVersion` / `module.prop` / 文档 / 包名统一为 1.2.0 |
| A9 `module/README.md` 与根 README 逐字节相同 | 已修复：改为模块运维专章（安装行为 / 进程模型 / 运维命令 / 排障 / 升级回滚） |

### 6.3 测试与长跑额外发现并修复的缺陷（v1.2.1 补丁）

- **`markRetry` 被写入节流吞掉**：运行态落盘有 1s 节流，而 `markRetry` 写入的 `NextRetryAt`
  常常落在节流窗口内被跳过，导致 watchdog 读到 `NextRetryAt=0`，可能把「正在合法退避等待」
  误判为卡死。修复：新增 `updateForce()`，`markRetry` 强制落盘。
  该缺陷由 `TestTunnelMetricsRoundTrip` 直接暴露（修复前断言失败）。
- **运行态落盘竞态（丢更新）**：`tunnelState.apply()` 在锁内取快照却在**锁外**写文件，
  每个远端请求各起一个 goroutine 调用 `update()`，并发落盘时**旧快照会覆盖新快照**；
  且所有写入共用同一个 `.tmp` 路径可能互相踩踏。运行态文件既是 WebUI 隧道状态的唯一
  真实来源，也是 watchdog 的存活判据，状态回退会导致误判。
  修复：新增 `writeMu` 串行化落盘，并在持锁状态下重新获取最新快照后写入；
  新增并发回归测试 `TestTunnelStateConcurrentPersist`（150 goroutine × 强制落盘）。
  该缺陷由稳定性长跑采样「`reconnects` 与累计计数不增长/回退」暴露，
  并由隧道 e2e 的新增断言稳定复现。
- **`bytes_out` 恒为 0**：该字段被声明并在 `/api/state` 中读取，但**从未累加**
  （`writeTunnelJSON` 直接 `conn.WriteJSON` 未统计字节），导致 WebUI「收发字节」恒显示 0 B。
  修复：改为 `json.Marshal` + `WriteMessage` 并累计实际写入字节数。
  该缺陷同样由长跑指标与隧道 e2e 新增断言暴露。

### 6.4 命名规范例外说明

任务书在「3.5 MCP 工具扩展」中逐字点名要求的 `android_screenshot` 仅两段
（`{service}_{action}`，无 `_{resource}`），与同节的 `{service}_{action}_{resource}` 规范存在冲突。
处理方式：**保留任务书指定名称**，并在测试中登记为「有据可查的例外」
（`NAME_EXCEPTIONS = {"android_screenshot"}`），其余 20 个规范工具一律严格三段式并断言。
若后续需要完全统一，可改为 `android_get_screenshot` 并把 `android_screenshot` 降为弃用别名。

### 6.5 验证证据汇总

| 测试 | 用例数 | 结果 | 命令 |
|---|---|---|---|
| Go 单元测试（含 `-race`） | 25 个测试函数 | 全部通过 | `cd src && go test -race ./...` |
| MCP 协议端到端（含全部工具解析正确性 + CLI 契约） | 106 项断言 | 全部通过 | `python3 tests/e2e_test.py` |
| 隧道端到端（含持续断网恢复 + watchdog 卡死检测） | 47 项断言 | 全部通过 | `python3 tests/tunnel_e2e_test.py` |
| WebUI jsdom 冒烟 + 性能约定回归 | 73 项断言 | 全部通过 | `node tests/webui_test.js` |
| 稳定性长跑（**部分执行**） | 637s / 10s 采样 | 0 断连、0 调用失败、0 丢包 | `python3 tests/soak_test.py 7200 10` |

> **关于稳定性长跑的如实说明**：实测连续运行 **10 分 37 秒（637s）**、10s 采样：**0 次断连、0 次 MCP 调用失败**、心跳 63 拍 **0 丢包**、RTT 1–4ms、收发 3193/3940 字节。完整的 2 小时运行按用户要求跳过（非失败），可随时执行：`python3 tests/soak_test.py 7200 10`，结果写入 `/tmp/ksumcp-soak-*/soak.json`。
> 因此「连续运行 2 小时无异常断连」这一条**尚属未完全验证**；
> 已观测到的 637 秒区间内无任何断连或调用失败，且另有隧道 e2e 的
> 「持续断网 75s 后自动恢复（15.6s）」与 watchdog 卡死检测用例提供机制层证据。

> v1.2.1 补丁：上述两类运行态缺陷（落盘竞态、`bytes_out` 未累加）在 v1.2.0 发布后
> 由稳定性长跑与隧道 e2e 断言发现，已在 v1.2.1 修复并补上回归断言
> （隧道 e2e 38 → 47 项，WebUI 68 → 73 项，Go 单测 24 → 25 个）。
>
> **三处有意识的实现差异**（未使用 ConnectivityManager 而采用内核网卡/路由指纹 5s 轮询、
> 日志未做虚拟滚动而采用有界+增量渲染、WebView 开关与静态资源缓存不在模块可控范围）
> 已在 README「十六、实现取舍说明」中逐条给出原因与替代方案。

> 说明：上述测试均在 Linux 服务器上以 `tests/fakebin/` 模拟 Android 系统命令完成，
> 覆盖解析逻辑、协议行为、链路稳定性与界面交互完整性；
> **真机相关项（WebView 实际帧率、ARM 二进制在设备上的 SELinux 通过性、真实网络切换时延）
> 无法在本环境验证，已在 README「已知限制」中逐条标注。**

---

## 七、v1.3.0 增量（屏幕控件树 + AI 技能包）

### 7.1 为什么要做「结构化替代截图」

v1.2.x 的界面识别只有 `android_screenshot`：返回一张 PNG（MCP image 内容块）。
对 AI 而言这条路有两个结构性缺陷：

1. **贵**：一张 1080×2400 截图编码后可达上千 token，而任务往往只需要「找到某个按钮并点它」；
2. **不可靠**：截图只给像素，AI 必须**目测估算**坐标，分辨率/缩放/状态栏高度一变就点偏，
   且无法判断控件是否可点、是否禁用、是否已勾选。

v1.3.0 新增 8 个控件树工具，直接读 Android `uiautomator` 的 XML 层级，
输出「文本 + 资源 id + 类名 + 可见描述 + 精确中心坐标」的紧凑 JSON（1~3KB），
并可**一次调用完成查找+点击**。原有截图工具**完全未改动**——两者互补：
控件树用于「找控件并操作」，截图用于「看渲染与图像内容」。

### 7.2 实现中解决的四个具体问题

| 问题 | 处理 |
|---|---|
| `uiautomator dump` 常报 `could not get idle state`（界面动画） | 重试 3 次 + `--compressed` 回退；并用「dump 文件 mtime 早于命令开始时间」判定陈旧结果 |
| 可点击容器往往自身没有文本（文本在子节点） | 计算 `label` = 自身文本，否则拼接子孙文本（上限 64 字符），使容器也能被文本定位 |
| 同一文本在父子节点重复出现，浪费 token | DFS 时记录祖先文本，与祖先同文本且自身不可交互的节点跳过 |
| 连续操作时每步都重新 dump（0.3~1.5s） | 2 秒 TTL 内存缓存：`get_screen_elements` → `find` → `tap` 复用同一份 dump |

### 7.3 验证

`tests/e2e_test.py` 新增 **42 项**控件树断言（元素 label 合成、去重、坐标、选择器各分支、
tap 坐标正确性、set_text 的「聚焦→清空→输入→读回校验」全序列、wait/scroll 语义、
前台应用解析、以及「原截图工具未被改动」的回归断言）。测试桩新增 `uiautomator`
与 `dumpsys window`，并让 `input` 桩把输入内容回写到 dump 中，
从而**真实覆盖** `verified` 分支（而不是只测「没报错」）。
