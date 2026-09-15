# KSU MCP Server v1.2.1

> **建议所有 v1.2.0 用户升级到本版本。** 本版含 2 项缺陷修复 + 4 项收尾完善。 v1.2.0 的二进制存在两个已修复的缺陷（详见下）。
> 功能、协议、工具集与 v1.2.0 完全一致：协议 **MCP 2025-11-25** · 工具 **32 个**（21 规范命名 + 11 弃用别名）·
> 隧道 **10s 心跳 / 35s 判死**。

一键安装：下载下方 `ksu-mcp-server-v1.2.1.zip`，在 KernelSU Manager → 模块 → ＋ 覆盖安装即可，
`/data/adb/ksu_mcp` 配置自动保留。

---

## 修复的问题

### 1. 隧道运行态落盘竞态（丢更新 / 状态回退）

**现象**：`tunnel.runtime.json` 中的累计指标（转发请求数、收发字节）会**回退到旧值**，
WebUI 的隧道指标与 watchdog 的存活判据因此可能读到过期快照。

**根因**：v1.2.0 的 `tunnelState.apply()` 在锁内取快照，却在**锁外**写文件。
每个远端请求都会起一个 `handleTunnelRequest` goroutine 调用 `update()`，
多个 goroutine 并发落盘时**旧快照会覆盖新快照**；且所有写入共用同一个
`tunnel.runtime.json.tmp` 路径，可能互相踩踏。

**修复**：新增 `writeMu` 串行化落盘，并在**持锁状态下重新获取最新快照**后写入，
保证最后落盘的一定是最新数据。新增并发回归测试
`TestTunnelStateConcurrentPersist`（150 goroutine × 强制落盘，断言计数无丢失、
文件为合法 JSON），整套单测在 `go test -race` 下通过。

> 为什么这条重要：运行态文件既是 WebUI 隧道状态的**唯一真实来源**，
> 也是 watchdog「隧道是否卡死」的判据。状态回退会让看门狗误判或漏判。

### 2. `bytes_out` 恒为 0（WebUI「收发字节」显示错误）

**现象**：设备端 WebUI 内网穿透面板的「收发」标签恒显示 `0 B`；`tunnel.runtime.json`
的 `bytes_out` 始终为 0。

**根因**：`tunnelMetrics.BytesOut` 字段被声明并在 `/api/state` 中读取，但**从未累加**——
`writeTunnelJSON()` 直接调用 `conn.WriteJSON()`，没有统计实际写入字节数。

**修复**：`writeTunnelJSON()` 改为 `json.Marshal` + `WriteMessage`（顺带避免 `WriteJSON`
的二次序列化），成功后累计真实写入字节数。

### 3. 测试增强（防止上述两类缺陷回归）

- `tests/tunnel_e2e_test.py` 新增 3 条断言：服务端与设备端的 `bytes_out` 均非零、
  设备端累计转发请求与响应数正确（用例数 38 → **41**）。
- `src/main_test.go` 新增并发落盘回归测试（测试函数 24 → **25**）。

---

## 资产重切说明

v1.2.1 首次发布后，仓库做了一次**行为不变**的前端死代码清理
（移除 WebUI 中未使用的 `LOG_TEXT` 缓存、`panel` 变量、`currentTab` 状态与冗余 `dataset.label`），
并据此重新打包了 `ksu-mcp-server-v1.2.1.zip`（仅 `webroot/index.html` 变化，二进制未变）。

为保证 **tag ⟷ 仓库源码 ⟷ Release 资产** 三者可复现一致，已将 `v1.2.1` tag 前移到包含该清理的提交，
并重新上传全部资产。清理后四套测试已全部重跑：Go 单测 25（`-race`）、MCP e2e 106、
隧道 e2e 41、WebUI 68，全部通过。

---

## 同时完成的收尾完善

### 4. 补齐心跳丢包指标（任务 3.6 要求「延迟、丢包、重连次数」）

此前只有延迟与重连次数，缺「丢包」。现按 ping/pong 配对统计：
上一拍 ping 未在下一拍发出前收到 pong 即计一次丢包。设备端输出
`ping_sent` / `ping_lost` / `ping_loss_percent`，服务端输出
`pingsSent` / `pingsMissed` / `lossPercent`；
设备端 WebUI 与服务端管理台均以「心跳丢包 x% (n/m)」展示。

### 5. Token 归位到「运行状态」面板

按任务 3.2 的面板划分（运行状态面板 = 服务状态 / watchdog / 端口 / **Token**），
把 Token 的显示 / 复制 / 重新生成从「连接信息」移入「运行状态」；
「连接信息」只保留本机 / 局域网 / 公网三级地址与各自的鉴权提示。

### 6. 日志改增量渲染

新增内容以旧内容为前缀时，只对文本节点 `appendData` 增量（复杂度 O(增量行)），
仅在日志轮转/截断或超限时整段重写，并保持「原本贴底才自动贴底」。
吸顶栏增加独立合成层提示（`transform:translateZ(0)`），滚动时只做层移动。

### 7. 测试增强

| 测试 | 变化 |
|---|---|
| Go 单测 | 新增并发落盘回归测试（24 → 25 个测试函数，`-race` 通过） |
| MCP e2e | 新增 CLI 契约测试（87 → **106** 项断言）：watchdog-status 退出码语义、start/restart/stop、ui-state、tools、logs --source |
| 隧道 e2e | 新增 3 条丢包/计数断言 + **持续断网 75s 恢复**测试（38 → **47** 项断言） |
| WebUI jsdom | 新增丢包指标、Token 面板归位、增量渲染不变量断言（68 → **73** 项断言） |

两处测试断言不再写死版本号（改为语义化版本形态校验 / 从 `server.js` 解析），
避免每次发版都要改测试。

---

## 实现取舍说明（有意识的差异）

1. **未使用 ConnectivityManager**：本模块是纯 Go、`CGO_ENABLED=0` 静态编译的内嵌二进制，
   不能依赖 NDK/JNI 运行时。替代方案为每 5s 读取内核网卡地址表 + 默认路由合成
   「网络环境指纹」，指纹变化即强制重连。对「触发重连」这一目标而言与
   ConnectivityManager 等价（其事件最终也反映为网卡/路由变化），且无 JNI 开销。
2. **日志未做虚拟滚动**：`<pre>` 内是单个文本节点；虚拟滚动需拆成数百个 DOM 节点并管理
   回收，移动端常量开销反而更高。改为「行数上限 300 + 增量 `appendData`」，
   渲染量有界且复杂度为 O(增量)。
3. **WebView 开关与静态资源缓存不在模块可控范围**：WebView 配置由 KernelSU Manager 的
   `WebUIActivity` 决定；WebUI 是单文件自包含 HTML（零外部请求），无需缓存策略。
   模块侧通过 CSS（去掉 backdrop-filter/固定背景、动画只用 transform/opacity、
   吸顶栏独立合成层、`contain` 约束重排）保证默认配置下也流畅。

详见仓库 `README.md` 第十六节。

---

## 与 v1.2.0 的关系

| 项 | v1.2.0 | v1.2.1 |
|---|---|---|
| 功能 / 协议 / 工具集 | 相同 | 相同 |
| 隧道运行态落盘 | 并发丢更新 | **已修复** |
| `bytes_out` 指标 | 恒为 0 | **已修复** |
| 自动化测试 | 87 + 38 + 68 + 24 | **106** + **47** + **73** + **25** |

> v1.2.0 的 Release 与 tag 保留不动（便于追溯），但建议直接使用 v1.2.1。

---

## 升级注意事项

1. 覆盖安装 zip 即可，配置与数据目录自动保留；服务端无需变更（服务端 `tunnel-server/`
   在 v1.2.0 已完成 10s 心跳改造，v1.2.1 未改服务端）。
2. 安装后建议进 WebUI 确认隧道面板的「收发字节」开始正常增长。
3. 回滚：重新安装 v1.2.0 或更早版本 zip 均可（运行态文件版本无关）。

## 已知限制

与 v1.2.0 一致，请见 [v1.2.0 Release Notes](https://github.com/HZY-520/KSU-MCP/releases/tag/v1.2.0)，
其中「本版本未在真机上验证的部分」同样适用于 v1.2.1：

- KernelSU Manager 内嵌 WebView 的实际滚动帧率；
- `file://` 页面到 `http://127.0.0.1` 的跨源 `fetch` 可用性
  （不可用时自动回落 `ksu.exec 'mcpd ui-state'`，功能完整、仅性能略降）；
- arm64 / arm 二进制在真机上的 SELinux 通过性；
- 真实 WiFi↔4G 切换、飞行模式恢复的时延；
- `screencap` / `input` / `svc` 在各厂商 ROM 上的可用性差异。

---

## 验证情况

| 测试 | 用例数 | 结果 |
|---|---|---|
| Go 单元测试（含 `-race`） | 25 个测试函数 | 全部通过 |
| MCP 协议端到端（含 CLI 契约） | 106 项断言 | 全部通过 |
| 隧道端到端（含持续断网恢复） | 47 项断言 | 全部通过 |
| WebUI jsdom 冒烟 + 性能约定回归 | 73 项断言 | 全部通过 |
| 稳定性长跑（**部分执行，2 小时按用户要求跳过**） | 637s / 10s 采样 | 0 断连 / 0 调用失败 / 0 丢包 |

### 关于「连续运行 2 小时」验收项的如实说明

实测连续运行 **10 分 37 秒（637s）**、10s 采样：**0 次断连、0 次 MCP 调用失败**、心跳 63 拍 **0 丢包**、RTT 1–4ms、收发 3193/3940 字节。完整的 2 小时运行按用户要求跳过（非失败），可随时执行：`python3 tests/soak_test.py 7200 10`，结果写入 `/tmp/ksumcp-soak-*/soak.json`。

因此「连续运行 2 小时无异常断连」目前**未完全验证**，仅验证了其中 637 秒。
机制层面的证据另有：隧道 e2e 的「持续断网 75s 后自动恢复（实测 15.6s）」、
watchdog 隧道卡死检测（21s 内强杀重启）、以及心跳 10s/判死 35s 的常量不变量单测。

## 构建产物

| 文件 | 说明 |
|---|---|
| `ksu-mcp-server-v1.2.1.zip` | **KernelSU / Magisk 模块包**（含 arm64 + arm 双架构二进制、安装脚本、WebUI） |
| `mcpd-arm64-v1.2.1` | 独立 arm64 二进制（aarch64，静态链接，已剥离符号） |
| `mcpd-arm-v1.2.1` | 独立 arm 二进制（ARM EABI5，静态链接，已剥离符号） |
| `ksu-mcp-tunnel-server-v1.2.0.zip` | 公网穿透**服务端**包（server.js / admin.html / 配置样例 / Nginx 样例 / 部署手册；不含 config.json 与 node_modules）。服务端代码在 v1.2.1 未变动，故仍标记为 v1.2.0 |
