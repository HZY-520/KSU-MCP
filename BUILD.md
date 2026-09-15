# KSU MCP 全栈 · 构建与发布说明（v1.3.0）

本仓库为 **KSU MCP Server v1.3.0 全栈**源码：手机端 KernelSU/Magisk 模块（Go）+ 公网穿透服务端（Node.js）。

## 一、v1.3.0 相对 v1.2.1 的变更

**新增 8 个屏幕控件树工具**（`src/uiauto.go`）：读取 Android `uiautomator` 控件树 XML，
解析展平后返回**结构化元素**（文本 / 资源 id / 类名 / 可见描述 / 精确中心坐标），
用结构化数据替代截图做界面识别；原截图工具保持不变。

- `android_get_screen_elements`：界面识别首选（1~3KB JSON 替代上千 token 的截图）
- `android_find_element`：按选择器精确查找
- `android_tap_element`：按选择器一次点击（查找+算坐标+点击合一）
- `android_set_element_text`：按选择器写入文本（聚焦→清空→输入→读回校验）
- `android_wait_for_element`：等待控件出现/消失（设备端轮询）
- `android_scroll_to_element`：滚动查找控件
- `android_dump_ui_hierarchy`：完整控件树（JSON/XML，疑难场景）
- `android_get_foreground_app`：前台应用包名与 Activity

实现要点：`could not get idle state` 重试 + `--compressed` 回退；`label` 由子孙文本合成
（让可点击容器也能被文本定位）；与祖先同文本的冗余节点去重；2 秒内存缓存
（get→find→tap 只 dump 一次）。

**新增 AI 技能包** `skills/ksu-mcp/`：成本模型 / 决策树 / 选择器排序 / 参数纪律 / 排障清单 /
端到端示例，单独打包为 `ksu-mcp-skills-v1.3.0.zip` 随 Release 发布。

**测试**：MCP e2e 新增 42 项控件树断言；工具数量断言改为从二进制动态推导
（新增工具时测试无需改动）；测试桩新增 `uiautomator`/`dumpsys window`，
且 `input`/`uiautomator` 桩可模拟「输入后界面文本真的变了」，从而真实覆盖 verify 分支。

## 二、v1.2.1 相对 v1.2.0 的变更（补丁）

- **修复隧道运行态落盘竞态**：快照在锁内取、文件在锁外写，多 goroutine 并发落盘时
  旧快照覆盖新快照（丢更新），且共用同一 `.tmp` 路径可能互相踩踏。
  现改为 `writeMu` 串行化 + 写入前重新取最新快照；新增并发回归测试
  `TestTunnelStateConcurrentPersist`。
- **修复 `bytes_out` 恒为 0**：该字段从未累加，WebUI「收发字节」恒显示 0 B；
  `writeTunnelJSON()` 改为 `Marshal + WriteMessage` 并统计真实写入字节。
- 收尾完善：补齐**心跳丢包指标**（`ping_sent`/`ping_lost`/`ping_loss_percent`，
  服务端 `pingsSent`/`pingsMissed`/`lossPercent`）；Token 归位到「运行状态」面板；
  日志改增量 `appendData` 渲染；吸顶栏独立合成层。
- 测试增强：Go 单测 24 → **25** 个测试函数（`go test -race` 通过）；
  MCP e2e 87 → **106** 项断言（新增 CLI 契约测试）；
  隧道 e2e 38 → **47** 项断言（新增丢包断言与**持续断网 75s 恢复**测试）；
  WebUI jsdom 68 → **73** 项断言（新增丢包、Token 面板归位、增量渲染不变量）。
- 三处有意识的实现差异（未用 ConnectivityManager、日志未做虚拟滚动、
  WebView 开关与静态资源缓存不在模块可控范围）见 README 第十六节。

> v1.2.0 的 tag 与 Release 保留不动以便追溯。

## 三、v1.2.0 相对 v1.1.0 的变更

### 新增能力

- **MCP 协议协商至 2025-11-25**（兼容 2025-06-18 / 2025-03-26 / 2024-11-05），
  `initialize` 返回 `instructions` 向 Agent 说明工具命名与推荐调用顺序。
- **新增 21 个 `android_{action}_{resource}` 规范命名工具**（原 11 个扁平命名工具保留为弃用别名）：
  设备/电池/存储/网络信息、应用分页列表、进程列表、logcat、系统属性、设置读写、
  剪贴板读写、截屏（返回 **MCP image 内容块**）、文本/按键/点击/滑动注入、
  以及规范化的 shell/文件/目录工具。
- **设备端只读控制 API**：`GET /api/state`（含三网络地址与隧道质量指标）、`/api/logs`、`/api/tools`，
  带 CORS 支持，供 WebUI 走 `fetch` 秒级刷新（避免高频 fork shell）；隧道转发层拒绝 `/api/*`。
- **隧道运行态与质量指标**：`tunnel.runtime.json` 记录连接态/延迟/重连次数/收发字节/最后错误；
  服务端新增 `GET /api/metrics` 与设备级指标，管理台卡片展示。

### 修复

- **WebUI `.hidden` CSS 规则缺失** → v1.1.0 的「本机/局域网/公网」分段标签完全失效
  （三块面板同时平铺、点击无反应），现补规则并加 jsdom 回归断言。
- **WebUI 卡顿**：移除 `background-attachment:fixed`、卡片级 `backdrop-filter`、
  `box-shadow` 关键帧动画、JS 涟漪（`getBoundingClientRect` 强制回流）；开关改 `transform:translateX`；
  轮询降频 + 失焦暂停 + 失败退避；列表 keyed-diff；日志限 300 行并与上次内容比对。
- **WebUI 摆设组件**：修复操作按钮失败后永久卡在「处理中」；隧道状态由「仅判 PID」改为真实连接态；
  修复隧道 Token 输入框永不回填导致「保存/连接」必然报错（改为**留空即沿用已保存 Token**）；
  补齐工具列表面板与非 KernelSU 环境的降级横幅 + 写操作禁用。
- **连接稳定性**：隧道心跳 20s→10s、判死 75s→35s；重连退避 2s→30s 指数 + ±20% 抖动
  （v1.1.0 连上即复位导致退避失效）；网卡指纹轮询 15s→5s 并纳入默认路由；
  Streamable HTTP 会话加 TTL/容量上限/定期回收（修复内存泄漏）；GET 长流下发 SSE `retry` 实现断流自动重连。
- **隧道全场景可达**：watchdog 由「仅判 PID」升级为「存活 + 运行态新鲜度」双判定
  （用 `NextRetryAt` 区分合法退避与真卡死）；服务端心跳 30s→10s / 3 次丢包判死。
- **控制面外泄风险**：隧道转发层拒绝 `/api` 及其路径穿越写法（`/mcp/../api/state`）。
- 截屏临时文件不再无限增长（仅保留最近 5 张）；`markRetry` 曾被写入节流吞掉导致
  watchdog 误判（单测发现并修复）。

### 工程

- Go 源码从单文件 `main.go` 拆为 8 个职责文件 + 单元测试文件。
- 新增 3 套自动化测试（详见第五节）。
- 修复 shell 脚本用 `grep '"running": true'` 解析 JSON 的脆弱耦合，改用 `watchdog-status` 退出码。
- `module/README.md` 不再是根 README 的逐字节副本，改为模块运维专章。

## 四、目录结构

```
ksu-mcp/
├── README.md                  # 模块总说明
├── BUILD.md                   # 本文件
├── docs/
│   ├── analysis-and-diagnosis.md  # 现状分析与六大问题根因诊断报告
│   └── architecture.html          # 系统架构图（浏览器打开）
├── src/                       # 【客户端】mcpd Go 源码
│   ├── main.go                #   入口 / 配置 / 常量 / CLI 分发
│   ├── android.go             #   Android 能力采集层
│   ├── tools.go               #   MCP 工具注册表与实现
│   ├── transport.go           #   stdio / Streamable HTTP / SSE / 会话回收
│   ├── tunnel.go              #   隧道客户端 / 运行态 / 质量指标
│   ├── watchdog.go            #   进程守护
│   ├── control.go             #   daemon / 启停 / 状态 / 配置
│   ├── uiauto.go              #   屏幕控件树工具（v1.3.0 新增）
│   ├── api.go                 #   聚合状态与 WebUI 只读 API
│   ├── main_test.go           #   单元测试
│   └── go.mod / go.sum
├── module/                    # 【客户端】模块打包源
│   ├── module.prop            #   v1.2.0 / versionCode=3
│   ├── customize.sh           #   安装脚本
│   ├── service.sh             #   开机自启
│   ├── boot-completed.sh      #   开机完成兜底保活
│   ├── sepolicy.rule
│   ├── system/bin/mcpd        #   PATH 包装器
│   ├── webroot/index.html     #   WebUI 控制台
│   ├── bin/{arm64,arm}/mcpd   #   架构二进制（构建产物，不入库）
│   └── README.md              #   模块运维说明
├── tunnel-server/             # 【服务端】内网穿透 Node.js 服务
│   ├── server.js / admin.html
│   ├── package.json / package-lock.json
│   ├── config.example.json / nginx.conf.sample
│   └── README.md              #   部署手册（宝塔 / VPS）
└── tests/                     # 自动化测试
    ├── e2e_test.py            #   MCP 协议端到端（stdio + HTTP + 全工具解析）
    ├── tunnel_e2e_test.py     #   隧道端到端（真实 Node 服务端 + 真实 mcpd 客户端）
    ├── webui_test.js          #   WebUI jsdom 冒烟 + 性能约定回归
    ├── soak_test.py           #   稳定性长跑
    └── fakebin/               #   Android 命令模拟器（供 Linux 上验证解析逻辑）
```

> 二进制（`module/bin/{arm64,arm}/mcpd`）**不入库**，按第三节从源码重建；
> `tunnel-server/config.json`（含真实 Token）已在 `.gitignore` 中忽略。

## 五、构建客户端模块 zip（手机端）

```bash
# 0) 准备 Go 1.23+（仓库源码 go.mod 声明 go 1.23）
cd src && go mod tidy && cd ..

# 1) 编译双架构（注意：目标 GOOS=linux，Android 运行 Linux 用户态 ELF）
cd src
CGO_ENABLED=0 GOOS=linux GOARCH=arm64        go build -trimpath -ldflags "-s -w" -o ../module/bin/arm64/mcpd .
CGO_ENABLED=0 GOOS=linux GOARCH=arm   GOARM=7 go build -trimpath -ldflags "-s -w" -o ../module/bin/arm/mcpd .
cd ..
chmod 755 module/bin/arm64/mcpd module/bin/arm/mcpd

# 2) 本地可执行性冒烟（amd64 版，验证版本号与工具注册表）
cd src && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/mcpd-test . && cd ..
/tmp/mcpd-test version      # → 1.2.0
/tmp/mcpd-test tools        # → 32 个工具（21 规范 + 11 弃用）

# 3) 打包模块（zip 内必须直接是模块文件，不能再套一层目录；
#    正式发布包必须同时包含 arm64 与 arm 两个架构目录，便于同一 zip 兼容两种设备）
cd module && zip -r ../ksu-mcp-server-v1.3.0.zip . -x "*.DS_Store" && cd ..

# 3b) 校验 zip 结构（根目录应直接是 module.prop，且双架构二进制均存在）
unzip -l ksu-mcp-server-v1.3.0.zip | grep -E "module.prop|bin/(arm64|arm)/mcpd"
```

> 打包说明：`customize.sh` 在安装时按架构把 `bin/<arch>/mcpd` 移到 `bin/mcpd` 并删除另一架构目录。

编译要点：
- **TLS 证书校验**依赖 Android 系统 CA 目录（`/system/etc/security/cacerts`、
  `/apex/com.android.conscrypt/cacerts`、`/data/misc/keychain/cacerts-added`），
  mcpd 在代码内自动加载（`androidCACertPool`），无需额外配置；
- 隧道支持 `tunnel.ip` 直连兜底：手机 DNS 异常时直连服务端公网 IP，TLS 仍按域名校验。

## 六、构建/部署服务端（VPS / 宝塔）

```bash
cd tunnel-server
npm install                     # 依赖 ws ^8.18.0
cp config.example.json config.json
# 生成双 Token：openssl rand -hex 24 ×2，填入 devices.<设备名>.tunnelToken / clientToken
node server.js                  # 或 pm2 start server.js --name ksu-mcp-tunnel
```

部署详见 `tunnel-server/README.md`（宝塔 Node 项目 + Nginx 反代 + WebSocket Upgrade + SSL）。

## 七、测试与验收

```bash
# 1) Go 单元测试（命名规范 / 协议协商 / 路径隔离 / 退避抖动 / URL 推导 / 注册表一致性 / 并发落盘）
cd src && go test -race ./... && cd ..

# 2) MCP 协议端到端（stdio + Streamable HTTP + 会话生命周期 + 控制 API + 全工具解析正确性）
python3 tests/e2e_test.py

# 3) 隧道端到端（真实 Node 服务端 + 真实 mcpd 隧道客户端 + 流式分片 + 断线恢复 + 看门狗卡死检测）
cd tunnel-server && npm install && cd ..
python3 tests/tunnel_e2e_test.py

# 4) WebUI 冒烟（jsdom：交互完整性 / 无摆设组件 / 性能约定回归防护）
npm install jsdom
NODE_PATH=$PWD/node_modules node tests/webui_test.js

# 5) 稳定性长跑（默认 2 小时，10s 采样；结果写入 /tmp/ksumcp-soak-*/soak.json）
python3 tests/soak_test.py 7200 10
```

验收结论请以各测试脚本的最终汇总行为准（全部用例通过 / 失败明细）。

## 八、发布清单

1. 版本号三处同步：`src/main.go` 的 `appVersion`、`module/module.prop` 的
   `version`/`versionCode`、本文档与 README 标题。
2. 双架构二进制重建并确认 `file module/bin/*/mcpd` 为 ARM ELF（arm64 为 aarch64，arm 为 ARM EABI5）。
3. 打包 `ksu-mcp-server-v1.3.0.zip`，**必须包含** `bin/arm64/mcpd` 与 `bin/arm/mcpd`。
4. **打包服务端**（单独上传为 Release 资产，包名与 `server.js` 的 `VERSION`、Release 版本一致）：
   ```bash
   cd tunnel-server
   zip -q ../ksu-mcp-tunnel-server-v1.3.0.zip server.js admin.html package.json \
       package-lock.json config.example.json nginx.conf.sample README.md
   cd ..
   # 校验：不得包含 config.json（含真实双 Token）与 node_modules
   unzip -l ksu-mcp-tunnel-server-v1.3.0.zip
   unzip -Z1 ksu-mcp-tunnel-server-v1.3.0.zip | grep -E "config\.json$|node_modules" && echo "❌ 含敏感文件" || echo "✅ 干净"
   ```
5. **打包技能包**（单独上传为 Release 资产）：
   ```bash
   cd skills && zip -r ../ksu-mcp-skills-v1.3.0.zip ksu-mcp README.md && cd ..
   unzip -l ksu-mcp-skills-v1.3.0.zip | grep -E "SKILL.md|skill.json|reference/|examples/"
   ```
6. 确认不提交敏感信息：`tunnel-server/config.json`、任何真实 Token、`node_modules/`。
7. 创建 GitHub Release，上传模块 zip、双架构二进制、**技能包 zip**、**服务端 zip**，Release Notes 写明：
   新增功能、修复问题、升级注意事项、已知限制。

## 九、安全提醒

- 两个 Token（tunnelToken / clientToken）请用随机源生成并妥善保管，泄露立即更换；
- WebUI 管理台仅经 HTTPS 访问，连续 5 次登录失败锁定 IP 10 分钟；
- 手机端 `read_only` / `exec_allowlist` 可限制危险操作面；
- 设备端 `/api/*` 控制 API 不经隧道转发，本机 Token 不会外泄。
