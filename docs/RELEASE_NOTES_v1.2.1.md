# KSU MCP Server v1.2.1（补丁版）

> **建议所有 v1.2.0 用户升级到本版本。** v1.2.0 的二进制存在两个已修复的缺陷（详见下）。
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

## 与 v1.2.0 的关系

| 项 | v1.2.0 | v1.2.1 |
|---|---|---|
| 功能 / 协议 / 工具集 | 相同 | 相同 |
| 隧道运行态落盘 | 并发丢更新 | **已修复** |
| `bytes_out` 指标 | 恒为 0 | **已修复** |
| 自动化测试 | 87 + 38 + 68 + 24 | **106** + **41** + 68 + **25** |

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
| 隧道端到端 | 41 项断言 | 全部通过 |
| WebUI jsdom 冒烟 + 性能约定回归 | 68 项断言 | 全部通过 |
| 稳定性长跑（2 小时 / 10s 采样） | 见 `tests/soak_test.py` | 0 断连 / 0 调用失败 |

## 构建产物

| 文件 | 说明 |
|---|---|
| `ksu-mcp-server-v1.2.1.zip` | **KernelSU / Magisk 模块包**（含 arm64 + arm 双架构二进制、安装脚本、WebUI） |
| `mcpd-arm64-v1.2.1` | 独立 arm64 二进制（aarch64，静态链接，已剥离符号） |
| `mcpd-arm-v1.2.1` | 独立 arm 二进制（ARM EABI5，静态链接，已剥离符号） |
