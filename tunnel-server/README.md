# KSU MCP Tunnel Server（内网穿透服务端）

把设备上的 **KSU MCP Server** 通过公网域名暴露给远端 MCP 客户端：
设备端模块（mcpd tunnel）作为**客户端**主动外连本服务，无需路由器端口映射；
远端客户端访问 `https://n.huziyang.top/mcp/<device>` 即可到达设备。

```
远端 MCP 客户端 ──HTTPS──> n.huziyang.top（本服务） ──WebSocket──> 手机 mcpd tunnel ──> 手机本地 MCP Server
```

## WebUI 管理台（v1.1.0 内置）

浏览器打开 **`https://n.huziyang.top/admin`**（本包默认账号 **admin / admin123**，**首次登录后请立即在「管理密码」处更换**）。
新版管理台为卡片式 UI，**移动端自适应**，支持丝滑动画：

- 运行统计：设备在线数、转发请求 / 失败数、当前转发中（数字滚动动画）
- 设备管理：在线状态（IP、接入时间）、添加设备（**可自定义 tunnelToken / clientToken**，留空自动生成 48 位随机串）、
  一键复制端点与 Token（默认遮罩可点开）、重置单个/全部 Token、**踢下线**、删除设备
  （新增与重置 Token 会**自动写回 config.json 并立即生效**；重置 tunnelToken 后设备需用新 Token 重连）
- 运行日志：最近 200 条（设备上下线、登录、管理操作），支持自动刷新
- 安全：登录会话 7 天；连续 5 次密码错误锁定该 IP 10 分钟；Cookie HttpOnly + SameSite=Strict

> 请务必通过 **HTTPS** 访问 /admin；Nginx 已反代整站，`/admin`、`/api/` 自动覆盖，无需额外配置。

### v1.1.0 稳定性升级（建议与设备端模块同步升级）

- **流式转发协议**：设备连接时服务端下发 `hello` 握手，≥ v1.1.0 设备按 64KB 分帧回传
  （Streamable HTTP GET 长流 / 大响应实时传输，公网不掉线）；旧设备自动降级单帧。
- **断线快速失败**：设备离线瞬间终结其所有在途请求（502），不再干等 90s 超时。
- **竞态修复**：超时 / 设备响应 / 断线三方互斥收口，杜绝 `headers already sent` 崩溃。
- **自定义 Token**：`POST /api/devices` 支持 `tunnelToken` / `clientToken` 可选字段（16-128 位 `[A-Za-z0-9_.\-]`）。
- **新增 API**：`POST /api/devices/<device>/kick` 踢下线（设备端会自动重连）。

> 升级方式：上传本目录新文件覆盖旧目录（server.js / admin.html），`pm2 restart ksu-mcp-tunnel` 即完成；
> config.json 无需改动，旧配置继续生效。

## 一、宝塔面板部署（推荐，5 分钟）

> 本包已包含开箱即用的 `config.json`（默认端口 **8081**，已生成随机双 Token）。
> 若你沿用旧的 `/www/wwwroot/ksu-mcp-server` 目录，先**删除旧文件**再解压覆盖，避免旧 server.js 残留。

1. **上传解压**：宝塔 →「文件」→ `/www/wwwroot/` 下新建 `ksu-mcp-server`（或沿用旧目录）→ 上传本 zip 并解压。
2. **确认 config.json**：打开 `config.json`，把 `devices` 的键 `my-phone` 改成你的设备名（与手机端一致），两个 Token 已预生成可直接用；如想更换：
   ```bash
   openssl rand -hex 24   # → tunnelToken
   openssl rand -hex 24   # → clientToken
   ```
3. **装依赖**：宝塔「终端」执行
   ```bash
   cd /www/wwwroot/ksu-mcp-server
   npm install
   ```
4. **添加 Node 项目**：宝塔「网站」→「Node 项目」→「添加项目」：
   - 项目名称：`ksu_mcp_server`
   - 项目目录：`/www/wwwroot/ksu-mcp-server`
   - 启动文件：`server.js`（类型选 general / Node 项目均可）
   - Node 版本：v20+（面板已装 v24.20.0，直接选）
   - **端口：8081**（关键——面板用此端口生成反代；若旧项目还配着 8080，改成 8081 再启动）
5. **绑域名 n.huziyang.top**：项目设置里添加域名（面板会自动生成 `proxy_pass http://127.0.0.1:8081` 的反代）。
   - 确认域名解析已指向本服务器 IP（宝塔「DNS」或域名服务商处）。
   - **WebSocket 关键项**：`/tunnel` 的 location 必须带
     `proxy_set_header Upgrade $http_upgrade; proxy_set_header Connection "upgrade";`
     （完整参考见包内 `nginx.conf.sample`；宝塔 Node 项目模板默认已含，若没有就按样例补。）
6. **SSL**：宝塔「SSL」为 `n.huziyang.top` 一键申请 Let's Encrypt，开启强制 HTTPS。
7. **启动验证**：项目点「启动」→ 状态变「运行中」→ 日志应出现
   `KSU MCP Tunnel Server 已启动: http://0.0.0.0:8081` 与 `已注册设备: xxx`。

### 常见失败

| 现象 | 原因 | 处理 |
|---|---|---|
| `EADDRINUSE 8080` | 旧配置/旧进程还监听 8080 | 确认 config.json 端口=8081；删掉旧的 8080 进程/项目；面板项目端口同步改 8081 |
| `无法读取 .../config.json` | 目录里没有 config.json | 确认解压完整（本包已内置）；或 `cp config.example.json config.json` |
| 启动成功但域名 502 | 反代端口与监听端口不一致 | 核对 Nginx `proxy_pass` 是否为 `127.0.0.1:8081`（面板项目端口改后会自动重写） |

## 二、通用部署（VPS，非宝塔）

```bash
scp -r ksu-mcp-tunnel-server root@your-vps:/opt/ksu-mcp-tunnel
cd /opt/ksu-mcp-tunnel && npm install
# 编辑 config.json：设备名 + 双 Token
npm i -g pm2
pm2 start server.js --name ksu-mcp-tunnel && pm2 save && pm2 startup
```

## 三、域名 n.huziyang.top 与 HTTPS

- **反代（推荐）**：80/443 交给 Nginx/Caddy，`/tunnel` 保留 WebSocket Upgrade 头（见 `nginx.conf.sample`），Node 监听 127.0.0.1:8081。
- **直接 TLS**：在 `config.json` 填证书路径并让 Node 监听 443：
  ```json
  "tls": { "cert": "/etc/letsencrypt/live/n.huziyang.top/fullchain.pem",
           "key": "/etc/letsencrypt/live/n.huziyang.top/privkey.pem" }
  ```

## 四、设备端配置（手机模块）

手机 KernelSU WebUI「内网穿透」卡片：

| 项 | 值 |
|---|---|
| 服务器 | `wss://n.huziyang.top/tunnel` |
| 设备名 | 与 config.json 的 devices 键一致（如 `my-phone`） |
| Token | config.json 中该设备的 `tunnelToken` |

保存并「连接」后，`mcpd tunnel-status` 显示 running 即成功。

## 五、远端 MCP 客户端接入

- 类型：**Streamable HTTP**（Cherry Studio / ChatBox 均可）
- URL：`https://n.huziyang.top/mcp/<device>`（如 `https://n.huziyang.top/mcp/my-phone`）
- 鉴权：`Authorization: Bearer <clientToken>`

## 六、运维

```bash
pm2 logs ksu-mcp-tunnel        # 日志（设备上下线、转发错误）
curl https://n.huziyang.top/health   # 健康检查 → ok
```

## 七、安全须知

- 隧道把设备的 **root shell 能力**暴露到公网，Token 必须足够长且保管好；泄露后立即更换两个 Token 并重启服务。
- 设备离线时远端请求返回 502；`requestTimeout` 默认 90 秒。
- 服务端不解析 MCP 内容，只做字节级转发，会话（Mcp-Session-Id）由设备端维持。
