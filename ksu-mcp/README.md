# KSU MCP Server — 将 MCP 永久嵌入已 root 的 Android 设备

一套为 **KernelSU（兼容 Magisk）** 打造的模块，把 **MCP（Model Context Protocol）服务端** 永久嵌入设备：
开机自启、守护进程、WebUI 控制台、**Streamable HTTP（新版协议）+ SSE 双传输**、**局域网访问**、**内网穿透（设备为隧道客户端，配合 Node.js 服务端）**，
让 Cherry Studio / ChatBox / Claude 等 MCP 客户端在任何地方调用设备能力（root shell、文件、系统信息等）。

---

## 一、包含内容

| 组件 | 说明 |
|---|---|
| `bin/mcpd` | MCP 服务端二进制（Go 静态，arm64 / arm 双架构，安装时自动保留匹配架构） |
| `service.sh` | 开机自启（service 阶段，KernelSU 与 Magisk 通用；含隧道自动拉起） |
| `boot-completed.sh` | 开机完成兜底保活（KernelSU 专用阶段；含隧道保活） |
| `system/bin/mcpd` | systemless 包装器，安装后可在终端直接执行 `mcpd` |
| `webroot/index.html` | KernelSU Manager 内嵌 WebUI 控制台（状态 / 启停 / Token / 配置 / 局域网 / 隧道控制 / 日志） |
| `customize.sh` | 安装脚本：架构检测、配置初始化 |
| `tunnel-server/` | **Node.js 内网穿透服务端**（部署到公网 VPS，域名 `n.huziyang.top`） |

## 二、安装

1. 将 `ksu-mcp-server-v1.1.0.zip` 推送到手机（任意目录）。
2. 打开 **KernelSU Manager** → 底部「模块」→ 右上角「＋」→ 选择 zip 安装。
3. 安装完成后建议 **重启一次**（或稍等片刻，service.sh 会自动拉起服务）。
4. 在「模块」列表点击该模块，即可看到 **WebUI 控制台** 入口。
> WebUI 必须在 KernelSU Manager 内打开（其 WebView 会注入 `ksu` 桥接对象）；用普通浏览器打开无法执行命令。

> Magisk 用户同样可以直接安装；WebUI 功能为 KernelSU 专属，Magisk 下不影响核心服务。
> 升级自旧版本（v1.0.x）时，请先在 Manager 中**移除旧模块**再安装 v1.1.0；配置与数据目录 `/data/adb/ksu_mcp` 会自动保留。
> 卸载：Manager 中移除模块即可；残留数据目录 `/data/adb/ksu_mcp` 可手动删除。

## 三、快速验证

```sh
mcpd status            # 查看运行状态（JSON，含端口 / Token / 隧道段）
curl -s http://127.0.0.1:9123/health   # 健康检查（无需鉴权）
mcpd tunnel-status     # 隧道状态
```

## 四、MCP 客户端接入

**方式一：Streamable HTTP（新版协议，推荐）**
- 类型：`Streamable HTTP`
- URL：`http://127.0.0.1:9123/mcp`（局域网访问时填手机局域网 IP）
- 鉴权：`Bearer <Token>`（Token 在 WebUI 或 `mcpd token` 查看）

**方式二：SSE（旧协议，兼容老客户端）**
- 类型：`SSE`
- URL：`http://127.0.0.1:9123/sse`

**方式三：内网穿透（任意网络）**
- 远端 URL：`https://n.huziyang.top/mcp/<设备名>`
- 鉴权：`Bearer <服务端下发的 clientToken>`
- 详见「八、内网穿透」。

**方式四：stdio（Termux 本地）**
```sh
/data/adb/modules/ksu_mcp/bin/mcpd
```

## 五、内置工具（Tools）

| 工具 | 说明 |
|---|---|
| `device_info` | 品牌 / 型号 / Android 版本 / SDK / 内核 / SELinux / root 状态 |
| `exec_command` | root 执行 shell 命令（受超时、只读模式、允许列表约束） |
| `read_file` | 读文件（utf8 / base64 自适应） |
| `write_file` | 写 / 追加文件（只读模式下禁用） |
| `list_dir` | 列目录（名称 / 类型 / 大小 / 权限） |
| `app_list` | 列出已安装应用包名 |
| `battery_info` | 电量 / 状态 / 温度 / 电压 |
| `network_info` | 网卡 / IP / 主机名 / WiFi |
| `screenshot` | 截屏（返回 base64 PNG 与保存路径） |
| `clipboard_get` | 读取剪贴板文本 |
| `getprop` | 读取系统属性 |

## 六、配置与安全

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
    "token": ""
  }
}
```

| 配置项 | 作用 |
|---|---|
| `bind` | 默认仅监听 `127.0.0.1`。设为 `0.0.0.0` 开启局域网访问（WebUI「局域网访问」开关）。**监听非回环地址时强制要求 Token 鉴权，否则拒绝启动** |
| `token` | MCP 鉴权 Token，WebUI 可一键重新生成 |
| `read_only` | 设为 `true` 后禁用 `exec_command` / `write_file`，适合只做信息查询 |
| `exec_allowlist` | 非空时仅放行前缀匹配的命令（如 `["ls", "cat", "getprop"]`），其余全部拒绝 |
| `exec_timeout` | 命令执行超时（秒） |
| `tunnel` | 内网穿透配置：`enabled` 开启后开机自动拉起隧道客户端；`server` 为服务端 WebSocket 地址；`device` 为设备唯一标识（须与服务端登记一致）；`token` 为服务端下发的 tunnelToken |

命令行调整：

```sh
mcpd config-set --port 9123 --read-only true --allowlist-add "ls" --allowlist-add "cat"
mcpd config-set --lan true                                   # 开启局域网
mcpd config-set --tunnel-enable true --tunnel-server wss://n.huziyang.top/tunnel \
      --tunnel-device my-phone --tunnel-token <tunnelToken>  # 配置并启用隧道
```

关闭开机自启：`touch /data/adb/ksu_mcp/disabled`（删除该文件恢复）。

## 七、CLI 命令速查

```sh
mcpd                        # stdio 模式（默认）
mcpd daemon --detach        # Streamable HTTP + SSE 守护进程（后台）
mcpd daemon --port 9123 --bind 0.0.0.0 --token xxx   # 带参数前台启动（非回环必须带 Token）
mcpd stop                   # 停止守护进程
mcpd status                 # 运行状态（JSON）
mcpd tunnel --server wss://n.huziyang.top/tunnel --device my-phone --token <tunnelToken> --detach
mcpd tunnel-stop            # 断开隧道
mcpd tunnel-status          # 隧道状态（JSON）
mcpd token [--regen]        # 查看 / 重新生成 Token
mcpd logs [N]               # 查看最近 N 行日志
mcpd config-set ...         # 见上文
mcpd init-config            # 初始化配置文件
```

## 八、内网穿透（模块为客户端 + Node.js 服务端）

架构：**手机模块作为隧道客户端主动外连服务端**（WebSocket，无需端口映射），远端 MCP 客户端通过公网域名访问设备。

```
远端 MCP 客户端 ──HTTPS──> n.huziyang.top（Node 服务端） ──WebSocket──> 手机 mcpd tunnel ──> 手机本地 MCP Server
```

### 服务端部署（一次性）

1. 把项目 `tunnel-server/` 目录上传到 VPS：`scp -r tunnel-server root@your-vps:/opt/ksu-mcp-tunnel`
2. `cd /opt/ksu-mcp-tunnel && npm install`
3. 生成两个 Token：`openssl rand -hex 24`（tunnelToken，给设备）、`openssl rand -hex 24`（clientToken，给远端客户端）
4. 编辑 `config.json`（由 `config.example.json` 复制），登记设备：
   ```json
   { "port": 8080,
     "devices": { "my-phone": { "tunnelToken": "<tunnelToken>", "clientToken": "<clientToken>" } } }
   ```
5. 启动：`pm2 start server.js --name ksu-mcp-tunnel && pm2 save`
6. 配置域名 `n.huziyang.top` 反代到 127.0.0.1:8080（`nginx.conf.sample` 已含 WebSocket Upgrade 必需配置，SSL 用 certbot 签发）。

### 设备端启用

WebUI「内网穿透」卡片填写：服务器 `wss://n.huziyang.top/tunnel`、设备名（与 `devices` 键一致）、Token（tunnelToken），保存并连接；或 CLI：

```sh
mcpd config-set --tunnel-enable true --tunnel-server wss://n.huziyang.top/tunnel \
      --tunnel-device my-phone --tunnel-token <tunnelToken>
mcpd tunnel --detach
```

### 远端客户端接入

- 类型：Streamable HTTP；URL：`https://n.huziyang.top/mcp/my-phone`；鉴权：`Bearer <clientToken>`

> 安全提醒：隧道把设备的 root shell 能力暴露到公网，两个 Token 必须足够长、妥善保管；泄露后立即更换并重启服务端。

## 九、自行编译（可选）

需要 Go 1.23+（模块依赖 `github.com/gorilla/websocket`），源码位于 `src/`：

```sh
cd src
go mod tidy
# arm64（Android PIE）
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o mcpd-arm64 .
# arm 32 位（静态）
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "-s -w" -o mcpd-arm .
```

替换 `module/bin/<arch>/mcpd` 后重新打包 zip 即可。

## 十、目录布局

```
ksu-mcp/
├── src/main.go               # mcpd 源码（Streamable HTTP + SSE + 隧道客户端）
├── module/                   # KernelSU / Magisk 模块
│   ├── module.prop
│   ├── customize.sh
│   ├── service.sh
│   ├── boot-completed.sh
│   ├── sepolicy.rule
│   ├── system/bin/mcpd       # PATH 包装器
│   ├── webroot/index.html    # KernelSU WebUI
│   └── bin/{arm64,arm}/mcpd  # 架构二进制（安装时保留其一）
└── tunnel-server/            # Node.js 内网穿透服务端
    ├── server.js
    ├── package.json
    ├── config.example.json
    ├── nginx.conf.sample
    └── README.md             # 服务端部署文档
```

## 十一、已知限制

- 仅支持 arm64 / arm 设备（x86_64 模拟器需按第九节自行编译）。
- `screenshot` / `clipboard_get` 依赖系统 `screencap` / `cmd clipboard`，个别 ROM 可能不可用。
- WebUI 需要新版 KernelSU Manager 支持 WebUI 特性。
- 隧道单设备单连接：同设备新连接会顶替旧连接（支持断线自动重连，2s 起步指数退避，上限 30s）。
- Streamable HTTP 会话由设备端维护：`Mcp-Session-Id` 经隧道原样中继，同一会话的多次调用由设备端维持状态。
