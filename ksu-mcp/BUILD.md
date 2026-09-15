# KSU MCP 全栈源码包 · 构建说明

本包为 **KSU MCP Server v1.0.0 全栈**源码：手机端 KernelSU/Magisk 模块（Go）+ 公网穿透服务端（Node.js）。

## 目录结构

```
ksu-mcp-src-v1.0.0/
├── README.md                  # 模块总说明（功能 / 安装 / 使用）
├── BUILD.md                   # 本文件：构建与打包
├── docs/architecture.html     # 系统架构图（浏览器打开）
├── src/                       # 【客户端】mcpd Go 源码
│   ├── main.go                #   主程序（Streamable HTTP + SSE + 隧道客户端 + WebUI 桥）
│   ├── go.mod                 #   依赖：github.com/gorilla/websocket v1.5.3
│   └── go.sum
├── module/                    # 【客户端】模块打包源（安装脚本 + WebUI + 配置）
│   ├── module.prop            #   模块元信息（v1.0.0 / versionCode=1）
│   ├── customize.sh           #   安装脚本：架构检测 / 二进制部署 / 初始化配置
│   ├── service.sh             #   开机自启（service 阶段，含隧道自动拉起）
│   ├── boot-completed.sh      #   开机完成兜底保活（含隧道保活）
│   ├── sepolicy.rule          #   SELinux 规则补充
│   ├── system/bin/mcpd        #   systemless 包装器（暴露 mcpd 命令）
│   ├── webroot/index.html     #   KernelSU WebUI 控制台
│   └── README.md
└── tunnel-server/             # 【服务端】内网穿透 Node.js 服务
    ├── server.js              #   设备接入 / 转发 / 心跳 / WebUI API
    ├── admin.html             #   管理台页面（/admin，登录鉴权）
    ├── package.json           #   Node 依赖：ws ^8.18.0
    ├── package-lock.json
    ├── config.example.json    #   配置模板（含 admin 账号、设备与双 Token）
    ├── nginx.conf.sample      #   Nginx 反代 + WebSocket Upgrade 示例
    └── README.md              #   部署手册（宝塔 / VPS）
```

> 二进制（module/bin/arm64、arm 下的 mcpd）与运行时配置（tunnel-server/config.json，含真实
> 双 Token）**不入源码包**，按下方步骤从源码重建。

## 一、构建客户端模块 zip（手机端）

```bash
# 1. 编译三个架构（要求 Go 1.23+）
cd src
GOOS=linux GOARCH=amd64 go build -o /tmp/mcpd-test .      # 本地测试用
GOOS=linux GOARCH=arm64 go build -o ../module/bin/arm64/mcpd .
GOOS=linux GOARCH=arm GOARM=7 go build -o ../module/bin/arm/mcpd .
chmod 755 ../module/bin/arm64/mcpd ../module/bin/arm/mcpd
cd ..

# 2. 打包模块（customize.sh 安装时会把对应架构二进制移到 bin/mcpd）
cd module && zip -r ../ksu-mcp-server-v1.0.0.zip . -x "*.DS_Store" && cd ..
```

编译要点：
- 目标 **GOOS=linux**（KernelSU/Magisk 在 Android 上运行 Linux 用户态 ELF）；
- TLS 证书校验依赖 **Android 系统 CA 目录**（/system/etc/security/cacerts 等），
  mcpd 已在代码内自动加载（`androidCACertPool`），无需额外配置；
- 隧道支持 `tunnel.ip` 直连兜底：手机 DNS 异常时直连服务端公网 IP，TLS 仍按域名校验。

## 二、构建服务端 zip（VPS / 宝塔）

```bash
cd tunnel-server
npm install                     # 安装依赖（ws）
# 生成配置：cp config.example.json config.json
# 生成双 Token：openssl rand -hex 24 ×2，填入 devices.<设备名>.tunnelToken / clientToken
node server.js                  # 或 pm2 start server.js --name ksu-mcp-tunnel
```

部署详见 `tunnel-server/README.md`（宝塔 Node 项目 + Nginx 反代 + WebSocket Upgrade + SSL）。

## 三、端到端链路

```
MCP 客户端 ──HTTPS──> n.huziyang.top ──(Nginx:443)──> Node 服务端(8081)
    ──WebSocket──> 手机 mcpd tunnel（直连兜底 IP）──> 本地 MCP Server(127.0.0.1:9123)
```

- 手机端配置：`/data/adb/ksu_mcp/config.json` 的 `tunnel` 段（server/device/token/ip）
- 远端调用：`https://n.huziyang.top/mcp/<device>` + `Authorization: Bearer <clientToken>`
- 管理台：`https://n.huziyang.top/admin`（默认 admin/admin123，首次登录请修改）

## 四、安全提醒

- 两个 Token（tunnelToken / clientToken）请用随机源生成并妥善保管，泄露立即更换；
- WebUI 管理台仅经 HTTPS 访问，连续 5 次登录失败锁定 IP 10 分钟；
- 手机端 `read_only` / `exec_allowlist` 可限制危险操作面。
