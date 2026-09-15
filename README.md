# KSU MCP Server — 将 MCP 永久嵌入已 root 的 Android 设备

一套为 **KernelSU（兼容 Magisk）** 打造的模块，把 **MCP（Model Context Protocol）服务端** 永久嵌入设备：
开机自启、进程守护、WebUI 控制台、**Streamable HTTP（新版协议）+ SSE 双传输**、**局域网访问**、**内网穿透（设备为隧道客户端，配合 Node.js 服务端）**，
让 Cherry Studio / ChatBox / Claude 等 MCP 客户端在**任何网络环境**下调用设备能力（root shell、文件、系统信息、屏幕与输入注入等）。

> **当前版本：v1.2.1** · 协议：MCP **2025-11-25**（兼容 2025-06-18 / 2025-03-26 / 2024-11-05）· 工具：**32 个**（21 个规范命名 + 11 个弃用别名）

---

## 一、v1.2.1 补丁说明

v1.2.1 修复 v1.2.0 的两个缺陷，功能与工具集不变（**建议 v1.2.0 用户升级**）：

1. **隧道运行态落盘竞态**：`tunnel.runtime.json` 的快照在锁内取、文件在锁外写，
   多个 `handleTunnelRequest` goroutine 并发落盘时**旧快照会覆盖新快照**（丢更新、状态回退），
   且共用同一 `.tmp` 路径可能互相踩踏。运行态文件既是 WebUI 隧道状态的唯一真实来源，
   也是 watchdog 判定「是否卡死」的判据，回退会导致误判。现改为 `writeMu` 串行化 +
   写入前重新获取最新快照，并新增并发回归测试（`go test -race` 通过）。
2. **`bytes_out` 恒为 0**：该字段只被声明与读取、从未累加，导致 WebUI 内网穿透面板的
   「收发字节」恒显示 `0 B`。现将 `writeTunnelJSON()` 改为 `Marshal + WriteMessage`
   并累计真实写入字节数（顺带省掉 `WriteJSON` 的二次序列化）。

> v1.2.0 的 tag 与 Release 保留不动以便追溯，建议直接使用 v1.2.1。

## 二、v1.2.0 变更速览

| 方向 | 变更 |
|---|---|
| **工具扩展** | 新增 21 个 `android_{action}_{resource}` 规范命名工具（原 11 个扁平命名工具保留为**弃用别名**，老客户端不破坏）。含应用分页、进程列表、logcat、存储分区、网络详情、剪贴板读写、文本/按键/点击/滑动注入、10 类系统设置读写；`android_screenshot` 直接返回 **MCP image 内容块**（不再把 base64 塞进文本） |
| **协议** | `initialize` 做版本协商，服务端偏好 2025-11-25；新增 `instructions` 字段向 Agent 说明工具命名与推荐调用顺序 |
| **WebUI 性能** | 移除 `background-attachment:fixed`（滚动每帧重绘整屏渐变）、卡片级 `backdrop-filter`、`box-shadow` 关键帧、JS 涟漪；开关改 `transform`；轮询 6s 自适应 + 失焦暂停 + 失败退避；列表 keyed-diff；日志限 300 行 + 变更比对 |
| **WebUI 完整性** | 修复 `.hidden` CSS 规则缺失（v1.1.0 导致「本机/局域网/公网」分段标签**完全失效**）；修复按钮失败后永久卡在「处理中」；隧道状态改为**真实连接态**（非仅 PID）；补齐工具列表面板；补齐非 KernelSU 环境的**降级横幅 + 写操作禁用** |
| **三网络地址** | 新增 `/api/state` 与统一「连接信息」卡片，本机 / 局域网 / 公网三个层级各带传输类型、鉴权提示与一键复制 |
| **连接稳定性** | 隧道心跳 20s→**10s**、判死 75s→**35s**（两端一致）；重连退避 2s→30s 指数 + **±20% 抖动**（v1.1.0 连上即复位导致退避失效）；网卡指纹轮询 15s→**5s** 并纳入默认路由；Streamable HTTP 会话加 **TTL + 容量上限 + 定期回收**（修复长期运行内存泄漏）；GET 长流下发 SSE `retry` 提示实现断流自动重连 |
| **隧道全场景** | watchdog 由「仅判 PID」升级为**存活 + 运行态新鲜度**双判定（区分合法退避与真卡死）；隧道运行态落盘（连接态/延迟/重连次数/最后错误）；服务端 30s→**10s 心跳 / 3 次丢包判死**并暴露设备级质量指标；隧道转发层拒绝 `/api/*` 控制面（含路径穿越写法） |
| **工程** | Go 源码按职责拆分为 `main/android/tools/transport/tunnel/watchdog/control/api` 8 个文件；新增 **3 套自动化测试**（Go 单测 + MCP 协议 e2e + WebUI jsdom），共 190+ 断言 |

---

## 三、包含内容

| 组件 | 说明 |
|---|---|
| `bin/mcpd` | MCP 服务端二进制（Go 静态，arm64 / arm 双架构，安装时自动保留匹配架构） |
| `service.sh` | 开机自启（service 阶段，KernelSU 与 Magisk 通用；统一拉起 watchdog 守护） |
| `boot-completed.sh` | 开机完成兜底保活（KernelSU 专用阶段；确保 watchdog 运行） |
| `system/bin/mcpd` | systemless 包装器，安装后可在终端直接执行 `mcpd` |
| `webroot/index.html` | KernelSU Manager 内嵌 WebUI 控制台（运行状态 / 连接信息 / 内网穿透 / MCP 工具 / 配置 / 日志） |
| `customize.sh` | 安装脚本：架构检测、配置初始化、立即拉起守护 |
| `tunnel-server/` | **Node.js 内网穿透服务端**（部署到公网 VPS，域名 `n.huziyang.top`） |

## 四、安装

1. 将 `ksu-mcp-server-v1.2.1.zip` 推送到手机（任意目录）。
2. 打开 **KernelSU Manager** → 底部「模块」→ 右上角「＋」→ 选择 zip 安装。
3. 安装完成后建议 **重启一次**（或稍等片刻，service.sh 会自动拉起服务）。
4. 在「模块」列表点击该模块，即可看到 **WebUI 控制台** 入口。

> WebUI 必须在 KernelSU Manager 内打开（其 WebView 会注入 `ksu` 桥接对象）。
> 用普通浏览器打开会在页面顶部显示**只读降级横幅**并禁用所有写操作按钮（预期行为，不是 bug）。

> Magisk 用户同样可以直接安装；WebUI 功能为 KernelSU 专属，Magisk 下不影响核心服务。
> 从 v1.1.0 升级：可直接覆盖安装（配置与 `/data/adb/ksu_mcp` 自动保留）；建议升级后进入 WebUI 确认隧道已连接。
> 卸载：Manager 中移除模块即可；残留数据目录 `/data/adb/ksu_mcp` 可手动删除。

## 五、快速验证

```sh
mcpd status            # 运行状态（JSON：端口/Token/三网络地址/watchdog/隧道质量指标/工具数）
mcpd tools             # 列出全部 MCP 工具（× 标记为弃用别名）
curl -s http://127.0.0.1:9123/health   # 健康检查（无需鉴权）
mcpd watchdog-status   # 守护状态（退出码 0=运行中 / 2=未运行）
mcpd tunnel-status     # 隧道状态（JSON：连接态/延迟/重连次数/流量/最后错误）
```

## 六、MCP 客户端接入（三个网络层级）

| 网络层级 | 地址 | 适用场景 | 鉴权 |
|---|---|---|---|
| **本机** | `http://127.0.0.1:9123/mcp` | 设备本机的 MCP 客户端（Termux 等） | `Bearer <本机 Token>` |
| **局域网** | `http://<设备局域网IP>:9123/mcp` | 同一 WiFi 下的其他设备（需开启「局域网访问」） | `Bearer <本机 Token>` |
| **公网** | `https://n.huziyang.top/mcp/<设备名>` | 任意有网环境（需开启隧道） | `Bearer <clientToken>` |

- 传输类型：**Streamable HTTP**（推荐）或 **SSE**（旧客户端，地址为 `/sse`）
- 本机 / 局域网 Token 在 WebUI「连接信息」卡片或 `mcpd token` 查看
- 公网 `clientToken` 由隧道服务端管理台 `/admin` 下发
- WebUI「连接信息」卡片对每条地址提供**传输类型 + 鉴权方式 + 一键复制**

**stdio（Termux 本地）**：

```sh
/data/adb/modules/ksu_mcp/bin/mcpd
```

## 七、内置工具（32 个）

### 7.1 规范命名工具（21 个，推荐使用）

命名遵循 MCP 2025-11-25 规范的 `{service}_{action}_{resource}` 三段式，`service` 固定为 `android`。

**信息采集（只读）**

| 工具 | 说明 |
|---|---|
| `android_get_device_info` | 品牌/型号/Android 版本/SDK/ABI/内核/SELinux/开机时长，以及 KernelSU、Magisk 检测与版本 |
| `android_get_battery_status` | 电量/状态/健康/温度/电压/电流/充电器信息（sysfs 缺失时回落 `dumpsys battery`） |
| `android_get_storage_info` | `df` 分区列表 + 关键路径 statfs 精确容量，支持挂载点过滤与伪文件系统开关 |
| `android_get_network_info` | 默认路由网卡与类型（wifi/cellular/ethernet/vpn）、IP、网关、DNS、WiFi SSID、运营商、制式、飞行模式/移动数据开关 |
| `android_list_packages` | 已安装应用包名，支持 `filter` / `state`(all/third_party/system/enabled/disabled) / **分页 `offset`+`limit`** |
| `android_get_running_processes` | 进程列表（PID/PPID/用户/RSS/VSZ/名称），支持过滤、按内存/虚拟内存/PID/名称排序与条数上限 |
| `android_get_logcat` | logcat 快照，支持 `tags` / `priority` / `buffer` / 整行关键字过滤 / 行数上限 |
| `android_get_prop` | 系统属性（单键或全部） |
| `android_get_setting` | 读取 10 类系统设置的当前值（与 `android_toggle_setting` 同一套键名） |
| `android_get_clipboard` | 读取剪贴板文本 |

**界面与输入**

| 工具 | 说明 |
|---|---|
| `android_screenshot` | 截屏并返回 **MCP `image` 内容块**（PNG）+ 保存路径元信息；支持 `display_id`；设备上最多保留 5 张 |
| `android_input_text` | 向当前焦点输入框注入文本（直接 exec 单参数，不经 shell，引号/分号安全） |
| `android_input_key` | 注入按键（HOME / BACK / ENTER / POWER 等） |
| `android_input_tap` | 按坐标点击 |
| `android_input_swipe` | 滑动手势（滚动 / 下拉通知栏 / 返回手势），可指定时长 |
| `android_set_clipboard` | 写入剪贴板 |

**配置与命令**

| 工具 | 说明 |
|---|---|
| `android_toggle_setting` | 切换系统设置：`wifi` `bluetooth` `airplane_mode` `mobile_data` `location` `auto_rotate` `stay_awake`（布尔）/ `brightness`(0-255) `volume_music/ring/alarm/notification`(0-15)（整数）/ `dnd`（枚举）。多候选命令依次尝试并**读回校验** |
| `android_exec_shell` | root 执行 shell 命令（受 `read_only`、`exec_allowlist`、超时约束），返回 stdout/stderr/退出码 |
| `android_read_file` | 读文件（UTF-8 自适应 / base64，可限字节数） |
| `android_write_file` | 写 / 追加文件（`read_only` 下禁用） |
| `android_list_dir` | 列目录（名称/类型/大小/权限） |

> 每个工具的 `description` 都面向 **AI Agent** 编写：说明「何时使用 / 参数含义 / 返回字段 / 失败与限制」，
> 因为 Agent 完全依赖该文本进行工具选择与参数填充。

**命名例外**：任务书逐字指定的 `android_screenshot` 仅两段（无 `_{resource}`），
作为**有据可查的例外**保留；其余 20 个规范工具均为严格三段式（测试中已断言）。

### 7.2 弃用别名（11 个，保留兼容）

`device_info` `exec_command` `read_file` `write_file` `list_dir` `app_list` `battery_info` `network_info` `screenshot` `clipboard_get` `getprop`

这些 v1.1.0 的扁平命名工具**继续可用**，但 `description` 首行会标注 `⚠️ 已弃用` 与替代工具名。
建议按对照表迁移：`device_info→android_get_device_info`、`exec_command→android_exec_shell`、
`app_list→android_list_packages`、`battery_info→android_get_battery_status`、`network_info→android_get_network_info`、
`screenshot→android_screenshot`、`clipboard_get→android_get_clipboard`、`getprop→android_get_prop`、
`read_file→android_read_file`、`write_file→android_write_file`、`list_dir→android_list_dir`。

## 八、配置与安全

配置文件：`/data/adb/ksu_mcp/config.json`

```json
{
  "port": 9123,
  "bind": "127.0.0.1",
  "token": "<自动生成，48位随机>",
  "exec_timeout": 30,
  "exec_allowlist": [],
  "read_only": false,
  "tunnel": {
    "enabled": false,
    "server": "wss://n.huziyang.top/tunnel",
    "device": "android-device",
    "token": "",
    "ip": ""
  }
}
```

| 配置项 | 作用 |
|---|---|
| `bind` | 默认仅监听 `127.0.0.1`。设为 `0.0.0.0` 开启局域网访问（WebUI「局域网访问」开关）。**监听非回环地址时强制要求 Token 鉴权，否则拒绝启动** |
| `token` | MCP 鉴权 Token，WebUI 可一键重新生成 |
| `read_only` | 设为 `true` 后禁用 `android_exec_shell` / `android_write_file`（含弃用别名），适合只做信息查询 |
| `exec_allowlist` | 非空时仅放行前缀匹配的命令（如 `["ls", "cat", "getprop"]`），其余全部拒绝 |
| `exec_timeout` | 命令执行超时（秒） |
| `tunnel` | 内网穿透：`enabled` 开启后开机自动拉起隧道；`server` 为服务端 WebSocket 地址；`device` 为设备唯一标识（须与服务端登记一致）；`token` 为服务端下发的 tunnelToken；`ip` 为 DNS 异常时的直连兜底（TLS 仍校验域名） |

命令行调整：

```sh
mcpd config-set --port 9123 --read-only true --allowlist-add "ls" --allowlist-add "cat"
mcpd config-set --lan true                                   # 开启局域网
mcpd config-set --tunnel-enable true --tunnel-server wss://n.huziyang.top/tunnel \
      --tunnel-device my-phone --tunnel-token <tunnelToken>  # 配置并启用隧道
```

关闭开机自启：`touch /data/adb/ksu_mcp/disabled`（删除该文件恢复）。

**安全提醒**：隧道把设备的 root shell 能力暴露到公网，两个 Token 必须足够长、妥善保管。
设备端 `/api/*` 控制 API **不会**经隧道转发（含 `/mcp/../api/state` 等穿越写法），本机 Token 不会外泄。

## 九、CLI 命令速查

```sh
mcpd                        # stdio 模式（默认）
mcpd start                  # 启动全部服务（拉起 watchdog）
mcpd restart                # 重启全部服务（stop + start，原子操作）
mcpd stop                   # 停止全部（tunnel / daemon / watchdog）
mcpd status                 # 运行状态（JSON，含三网络地址 / 隧道指标 / 工具数）
mcpd watchdog [--detach]    # 启动进程守护
mcpd watchdog-stop          # 停止守护
mcpd watchdog-status        # 守护状态（退出码 0=运行中 / 2=未运行）
mcpd daemon --detach        # Streamable HTTP + SSE 守护进程（后台）
mcpd ui-state               # WebUI 聚合状态（一次调用拿到全部数据）
mcpd ui-bootstrap           # WebUI 引导信息（port / token / api_base）
mcpd tools [--json]         # 列出 MCP 工具
mcpd tunnel --detach        # 启动隧道客户端（参数省略时读取配置）
mcpd tunnel-stop            # 断开隧道
mcpd tunnel-status          # 隧道状态（JSON）
mcpd token [--regen]        # 查看 / 重新生成 Token
mcpd logs [N] [--source mcpd|tunnel|watchdog]   # 查看日志
mcpd config-set ...         # 见上文
mcpd init-config            # 初始化配置文件
```

> **进程守护说明**：watchdog 每 10 秒巡检。
> daemon：`/health` 探活，连续 3 次失败强杀重启；
> 隧道：进程不存在即拉起；**进程存在但运行态文件超时未刷新且已过其计划重连时刻**则判定卡死并强杀重启
> （区分「正在合法退避等待」与「真卡死」，避免误杀）。
> 开机由 service.sh / boot-completed.sh 在 init 上下文拉起 watchdog，
> 因此**关闭 KernelSU Manager、离开 WebUI 页面都不会中断服务**。

## 十、内网穿透（模块为客户端 + Node.js 服务端）

架构：**手机模块作为隧道客户端主动外连服务端**（WebSocket，无需端口映射），远端 MCP 客户端通过公网域名访问设备。

```
远端 MCP 客户端 ──HTTPS──> n.huziyang.top（Node 服务端） ──WebSocket──> 手机 mcpd tunnel ──> 手机本地 MCP Server
```

### 10.1 服务端部署（一次性）

1. 把 `tunnel-server/` 上传到 VPS：`scp -r tunnel-server root@your-vps:/opt/ksu-mcp-tunnel`
2. `cd /opt/ksu-mcp-tunnel && npm install`
3. 生成两个 Token：`openssl rand -hex 24`（tunnelToken，给设备）、`openssl rand -hex 24`（clientToken，给远端客户端）
4. 复制 `config.example.json` 为 `config.json`，登记设备（也可登录 `/admin` 添加）：
   ```json
   { "port": 8081,
     "devices": { "my-phone": { "tunnelToken": "<tunnelToken>", "clientToken": "<clientToken>" } } }
   ```
5. 启动：`pm2 start server.js --name ksu-mcp-tunnel && pm2 save`
6. 配置域名 `n.huziyang.top` 反代到 `127.0.0.1:8081`（`nginx.conf.sample` 含 WebSocket Upgrade 必需配置）

> 登录管理台 `/admin` 即可添加设备，支持**自定义 tunnelToken / clientToken**（留空自动生成 48 位随机串），
> 并提供重置 Token、踢下线、移动端适配的卡片式 UI，以及**设备级连接质量指标**
> （心跳延迟、重连次数、请求/失败数、收发流量、本次/累计在线时长、离线原因）。

### 10.2 设备端启用

WebUI「内网穿透」卡片填写：服务器 `wss://n.huziyang.top/tunnel`、设备名（与 `devices` 键一致）、Token（tunnelToken），保存并连接；或 CLI：

```sh
mcpd config-set --tunnel-enable true --tunnel-server wss://n.huziyang.top/tunnel \
      --tunnel-device my-phone --tunnel-token <tunnelToken>
mcpd tunnel --detach
```

> **Token 不回显**：WebUI 的 Token 输入框始终留空，留空即表示「沿用已保存的 Token」。
> 这样既避免密钥往返浏览器，也不会出现「必须先重填 Token 才能保存」的卡点。

### 10.3 远端客户端接入

- 类型：Streamable HTTP；URL：`https://n.huziyang.top/mcp/<设备名>`；鉴权：`Bearer <clientToken>`

## 十一、稳定性设计参数

| 参数 | 值 | 说明 |
|---|---|---|
| 设备端心跳（WS ping） | **10s** | 携带时间戳负载用于测量 RTT |
| 设备端读超时（判死） | **35s** | 3 个心跳周期内未收到任何帧即断开重连 |
| 服务端心跳（WS ping） | **10s** | 与设备端一致 |
| 服务端判死 | **3 次丢包 ≈ 30s** | 超时即 `terminate`，并在途请求立即 502 快速失败 |
| 重连退避 | 2s → 30s 指数，**±20% 抖动** | 连接存活 <30s 不重置退避（避免「连上就断」的重连风暴） |
| 网卡指纹轮询 | **5s** | 指纹含网卡地址 + 默认路由，WiFi↔移动数据切换立即强制重连 |
| watchdog 巡检 | 10s | daemon：连续 3 次探活失败强杀重启 |
| 隧道卡死判定 | 运行态停滞 >45s 且已过计划重连时刻，连续 3 次 | 强杀并重启隧道进程 |
| Streamable HTTP 会话 | TTL 30 分钟 / 上限 512 个 / 每分钟回收 | 修复 v1.1.0 会话只增不减的内存泄漏 |
| SSE 断流恢复 | `retry: 3000` | 客户端断流后 3s 自动重连，会话在 TTL 内保持有效 |
| 隧道转发分片 | 64KB / 单帧阈值 256KB | 大响应与长流稳定性 |
| GET 长流空闲超时 | 30 分钟 | 其余请求 90s |

## 十二、自行编译与测试

需要 **Go 1.23+**（模块依赖 `github.com/gorilla/websocket`），源码位于 `src/`：

```sh
cd src
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o ../module/bin/arm64/mcpd .
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "-s -w" -o ../module/bin/arm/mcpd .
```

编译要点：
- 目标 **GOOS=linux**（KernelSU/Magisk 在 Android 上运行 Linux 用户态 ELF）；
- TLS 证书校验依赖 **Android 系统 CA 目录**，mcpd 已在代码内自动加载（`androidCACertPool`）；
- 隧道支持 `tunnel.ip` 直连兜底：手机 DNS 异常时直连服务端公网 IP，TLS 仍按域名校验。

### 测试套件（v1.2.0 新增，v1.2.1 增强）

```sh
# 1) Go 单元测试：命名规范 / 协议协商 / 路径隔离 / 退避抖动 / URL 推导 /
#    注册表一致性 / 运行态并发落盘无丢更新
cd src && go test -race ./...

# 2) MCP 协议端到端：stdio + Streamable HTTP + 控制 API + 全部工具解析正确性
#    + CLI 契约（start/restart/stop/watchdog-status 退出码、ui-state、tools、logs）
#    借助 tests/fakebin 下的 Android 命令模拟器，在 Linux 上验证真实解析逻辑
python3 tests/e2e_test.py

# 3) 隧道端到端：真实 Node 服务端 + 真实 mcpd 隧道客户端
cd tunnel-server && npm install && cd ..
python3 tests/tunnel_e2e_test.py

# 4) WebUI 冒烟（jsdom）：交互完整性 / 无摆设组件 / 性能约定回归防护
npm install jsdom && NODE_PATH=$PWD/node_modules node tests/webui_test.js

# 5) 稳定性长跑（默认 2 小时）
python3 tests/soak_test.py 7200 10
```

## 十三、目录布局

```
ksu-mcp/
├── src/                        # 【客户端】mcpd Go 源码（v1.2.0 按职责拆分）
│   ├── main.go                 #   入口 / 配置 / 常量 / CLI 分发
│   ├── android.go              #   Android 能力采集（命令、sysfs、/proc、设置项）
│   ├── tools.go                #   MCP 工具注册表与实现
│   ├── transport.go            #   stdio / Streamable HTTP / SSE / 协议协商 / 会话回收
│   ├── tunnel.go               #   隧道客户端 / 运行态 / 质量指标 / 心跳退避
│   ├── watchdog.go             #   进程守护（daemon + 隧道存活检测）
│   ├── control.go              #   daemon / 启停 / 状态 / 配置命令
│   ├── api.go                  #   聚合状态与 WebUI 只读控制 API
│   └── main_test.go            #   单元测试
├── module/                     # 【客户端】模块打包源
│   ├── module.prop             #   模块元信息
│   ├── customize.sh            #   安装脚本
│   ├── service.sh              #   开机自启
│   ├── boot-completed.sh       #   开机完成兜底保活
│   ├── sepolicy.rule           #   SELinux 规则补充
│   ├── system/bin/mcpd         #   PATH 包装器
│   ├── webroot/index.html      #   KernelSU WebUI 控制台
│   └── bin/{arm64,arm}/mcpd    #   架构二进制
├── tunnel-server/              # 【服务端】内网穿透 Node.js 服务
│   ├── server.js               #   设备接入 / 转发 / 心跳 / 指标 / WebUI API
│   ├── admin.html              #   管理台页面（/admin）
│   ├── nginx.conf.sample       #   Nginx 反代 + WebSocket Upgrade 示例
│   └── README.md               #   部署手册
├── tests/                      # 自动化测试（e2e / tunnel e2e / webui / soak + fakebin）
└── docs/
    ├── analysis-and-diagnosis.md   # 现状分析与六大问题根因诊断报告
    └── architecture.html          # 系统架构图（浏览器打开）
```

## 十四、已知限制

- 仅支持 arm64 / arm 设备（x86_64 模拟器需按第十一节自行编译）。
- `android_screenshot` / `android_get_clipboard` / `android_set_clipboard` 依赖系统 `screencap` / `cmd clipboard`，个别 ROM 可能不可用（工具会返回带原因的 `isError`）。
- `android_input_*` 依赖系统 `input` 命令；`input text` 在部分 ROM 上字面量 `%s` 会被解释为空格。
- `android_toggle_setting` 依 ROM 差异可能部分项失败（返回 `applied=false` 与候选命令的 stderr），务必以返回的 `state`（写入后读回）为准。
- WebUI 需要新版 KernelSU Manager 支持 WebUI 特性；普通浏览器打开为只读降级模式。
- 隧道单设备单连接：同设备新连接会顶替旧连接（客户端 10s WS 心跳 + 35s 读超时判死、2s 起步指数退避 + 抖动、上限 30s；网卡/路由变化 5s 内检测并强制重连）。
- 流式转发协议（长流 / 大响应分帧推送）需要服务端 ≥ v1.1.0；旧版服务端降级为单帧转发。
- Streamable HTTP 会话由设备端维护：`Mcp-Session-Id` 经隧道原样中继，同一会话的多次调用由设备端维持状态。
- **未在真机上验证的部分**（本次开发环境无 Android 设备，已如实标注）：KernelSU Manager 内嵌 WebView 的实际滚动帧率与 `file://`→`http://127.0.0.1` 跨源 `fetch` 可用性（不可用时自动回落 `ksu.exec`，功能不受影响，仅性能略降）；arm64/arm 二进制在真机上的 SELinux 通过性；真实 WiFi↔4G 切换与飞行模式恢复的时延；`screencap`/`input`/`svc` 在各厂商 ROM 上的可用性差异。

## 十五、故障排查

| 现象 | 排查方向 |
|---|---|
| MCP 客户端连不上（本机） | `mcpd status` 看 `running`/`port`；`curl -s 127.0.0.1:9123/health`；核对 Token |
| 客户端 401 | Token 不匹配；重新生成后需更新客户端配置 |
| 局域网连不上 | WebUI「运行配置 → 局域网访问」需打开并保存；确认 `bind=0.0.0.0`；确认手机与客户端同网段 |
| 公网连不上 | WebUI「内网穿透」看真实连接态与 `最近错误`；`mcpd tunnel-status` 看 `state`/`reconnects`/`last_error`；核对设备名与 tunnelToken 是否与服务端一致 |
| 公网频繁断连 | 看服务端管理台的**心跳延迟/重连次数**；设备端 `mcpd logs --source tunnel`；检查是否有其他设备顶替同名连接 |
| 服务莫名停止 | `mcpd logs --source watchdog` 看是否有卡死强杀记录；确认 `disabled` 标记未被误建 |
| WebUI 按钮点了没反应 | 确认从 KernelSU Manager 打开（普通浏览器会显示降级横幅并禁用写操作） |
| WebUI 卡顿 | 确认已用 v1.2.x 的 index.html；日志自动刷新默认关闭，仅在需要时打开 |
