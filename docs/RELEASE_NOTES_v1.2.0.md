# KSU MCP Server v1.2.0

> 协议 **MCP 2025-11-25** · 工具 **32 个**（21 规范命名 + 11 弃用别名）· 隧道 **10s 心跳 / 35s 判死**

macOS/Linux 用户升级命令（设备端）：直接在 KernelSU Manager 中安装下方
`ksu-mcp-server-v1.2.0.zip` 覆盖即可，`/data/adb/ksu_mcp` 配置自动保留。

---

## 一、新增功能

### 1. MCP 工具扩展：11 → 32 个（新增 21 个规范命名工具）

命名遵循 MCP 2025-11-25 的 `{service}_{action}_{resource}` 三段式（`service` 固定 `android`）：

| 分类 | 工具 |
|---|---|
| 信息采集（只读） | `android_get_device_info`（含 KernelSU/Magisk 检测与版本）、`android_get_battery_status`、`android_get_storage_info`、`android_get_network_info`、`android_list_packages`（**支持分页** `offset`/`limit` + `state` 过滤）、`android_get_running_processes`（支持过滤/排序/条数上限）、`android_get_logcat`（tag/优先级/缓冲区/关键字过滤）、`android_get_prop`、`android_get_setting`、`android_get_clipboard` |
| 界面与输入 | `android_screenshot`（**直接返回 MCP `image` 内容块**，不再把 base64 塞进 JSON 文本）、`android_input_text`、`android_input_key`、`android_input_tap`、`android_input_swipe`、`android_set_clipboard` |
| 配置与命令 | `android_toggle_setting`（**10 类系统设置**：wifi / bluetooth / airplane_mode / mobile_data / location / auto_rotate / stay_awake / brightness / volume_×4 / dnd，多候选命令依次尝试并**写入后读回校验**）、`android_exec_shell`、`android_read_file`、`android_write_file`、`android_list_dir` |

- 每个工具的 `description` 均**面向 AI Agent 编写**：说明何时使用、参数含义、返回字段、失败与限制；
  `inputSchema` 补齐 `enum` / `minimum` / `maximum` / `default` / `required` 约束。
- v1.1.0 的 11 个扁平命名工具（`device_info`、`exec_command`、`screenshot` 等）**继续可用**，
  但 `description` 首行标注 `⚠️ 已弃用` 与替代工具名，老客户端配置不受影响。

### 2. 协议升级与协商

- `initialize` 做协议版本协商：服务端偏好 **2025-11-25**，兼容 2025-06-18 / 2025-03-26 / 2024-11-05。
- 新增 `instructions` 字段，向 Agent 说明工具命名规范与推荐调用顺序
  （如「先 `android_screenshot` 观察 → `android_input_tap` 聚焦 → `android_input_text` 输入」）。

### 3. WebUI 三网络地址与工具面板

- 统一「连接信息」卡片，**本机 / 局域网 / 公网** 三个层级各自展示
  Streamable HTTP 与 SSE 两条地址，每条带传输类型、鉴权方式、不可用原因与**一键复制**。
- 新增「MCP 工具」面板：懒加载工具详情、支持按名称/说明过滤、标注弃用与替代关系。
- 新增只读控制 API：`GET /api/state`、`/api/logs?source=mcpd|tunnel|watchdog`、`/api/tools`。

### 4. 隧道连接质量指标

- 设备端 `tunnel.runtime.json`：连接态、心跳延迟、重连次数、连续失败、收发字节、转发请求数、最后错误。
- 服务端管理台新增每设备指标网格（心跳延迟 / 重连次数 / 请求-失败 / 收发流量 / 本次与累计在线时长 / 离线原因），
  并提供 `GET /api/metrics` 聚合视图。

### 5. 新 CLI 命令

`mcpd start` / `mcpd restart`（原子重启） / `mcpd ui-state` / `mcpd ui-bootstrap` /
`mcpd tools [--json]` / `mcpd logs [N] --source mcpd|tunnel|watchdog`。

---

## 二、修复的问题

### WebUI

1. **【致命】`.hidden` CSS 规则缺失** → v1.1.0 的「本机 / 局域网 / 公网」分段标签**完全失效**：
   点击无反应、三块面板同时平铺。现已补规则（并用 jsdom 测试锁定，防回归）。
2. **滚动与交互卡顿**：移除 `background-attachment:fixed`（滚动每帧重绘整屏渐变）、
   卡片级 `backdrop-filter`（6 次离屏模糊）、`box-shadow` 关键帧动画、JS 涟漪
   （每次点击 `getBoundingClientRect` 强制回流）；开关滑块由 `left` 改 `transform`；
   轮询由固定 5s 全量刷新改为 6s 自适应 + 失焦暂停 + 失败指数退避；
   地址列表改 keyed-diff；日志限 300 行并与上次内容比对后再写入。
3. **按钮点一次失败后永久卡在「处理中」**（`act()` 先覆盖 `innerHTML` 再取原文案）→ 统一 `btnBusy()` 管理。
4. **隧道状态是假的**（只看 PID 存活，进程卡在重连循环时仍显示「已连接」）→ 改读真实连接态。
5. **隧道 Token 输入框永不回填**，导致「保存配置 / 立即连接」必然报「Token 不能为空」，
   被感知为摆设按钮 → 改为**不回显、留空即沿用已保存值**（顺带避免密钥往返浏览器）。
6. **非 KernelSU 环境无降级提示**，按钮可点但必然失败 → 新增只读降级横幅并禁用全部写操作。
7. 直连 IP 字段永不回显、公网复制按钮静默失效、`copyToken()` 状态机错乱 → 一并修复。

### 连接稳定性

8. 隧道心跳 **20s→10s**、判死 **75s→35s**（与参考实践及服务端对齐）。
9. 重连退避「连上即复位」导致策略实际失效 → 改为连接存活 **<30s 不重置**，
   并叠加 **±20% 抖动**（避免多设备同时重连的惊群）。
10. 网卡指纹轮询 **15s→5s**，且指纹纳入**默认路由**（WiFi↔移动数据切换即使 IP 不变也能识别）。
11. Streamable HTTP 会话 `map` **只增不减的内存泄漏** → 加 TTL 30 分钟 + 上限 512 + 每分钟回收。
12. 重复 `initialize` 无条件新签会话 → 携带有效会话时复用。
13. `/mcp` GET 长流无断流恢复机制 → 下发 SSE `retry: 3000`，客户端断流后 3s 自动重连。

### 隧道与守护

14. **watchdog 只判 PID**，隧道进程卡死时永不干预 → 升级为「**存活 + 运行态新鲜度**」双判定：
    运行态停滞 >45s **且已过其计划重连时刻**（用 `NextRetryAt` 区分合法退避与真卡死）连续 3 次才强杀重启。
15. 服务端心跳 **30s→10s**，连续 **3 次丢包（≈30s）** 判死（v1.1.0 最坏 60s 才回收死连接）。
16. **控制面外泄风险**：隧道转发层现在拒绝 `/api/*` 及其**路径穿越写法**
    （`/mcp/../api/state`），设备本机 Token 不会经公网泄露。
17. 截屏临时文件无限增长 → 仅保留最近 5 张。
18. shell 脚本用 `grep '"running": true'` 解析 JSON（格式一改即失效）→ 改用 `watchdog-status` 退出码。
19. `markRetry` 写入被运行态落盘节流吞掉，会让 watchdog 误判卡死（**由单元测试发现**）→ 加 `updateForce()`。

---

## 三、升级注意事项

1. **设备端与服务端务必同步升级**：v1.2.0 的心跳参数（10s/35s）两端强耦合。
   服务端请用 `tunnel-server/` 下的新 `server.js` + `admin.html` 覆盖后 `pm2 restart ksu-mcp-tunnel`，
   `config.json` 无需改动。
2. **安装方式**：KernelSU Manager → 模块 → ＋ → 选择 `ksu-mcp-server-v1.2.0.zip` 覆盖安装，
   配置与 `/data/adb/ksu_mcp` 自动保留，建议安装后重启一次。
3. **客户端工具名迁移**（可选，不迁移也能用）：老配置里的 `device_info`、`exec_command`、
   `app_list`、`battery_info`、`network_info`、`screenshot`、`clipboard_get`、`getprop`、
   `read_file`、`write_file`、`list_dir` 均保留为弃用别名，建议按 README 的对照表迁移到 `android_*` 新名。
4. **WebUI 的隧道 Token 输入框现在是空的**（不再回显）：直接点「保存配置」**不会**清空已保存的 Token；
   仅在需要更换时才填入新 Token。
5. **首次升级后建议进 WebUI 确认隧道已连接**（v1.2.0 更换了心跳与运行态机制，
   连接态显示由「PID 存活」改为「真实 WS 连接 + 心跳新鲜度」）。
6. **回滚**：重新安装旧版 zip 即可；旧版不认识 `tunnel.runtime.json`（会忽略，无副作用）。

---

## 四、已知限制

- 仅支持 **arm64 / arm** 设备（x86_64 模拟器需按 `BUILD.md` 自行编译）。
- `android_screenshot` / `android_get_clipboard` / `android_set_clipboard` 依赖系统
  `screencap` / `cmd clipboard`，个别 ROM 可能不可用（会返回带原因的 `isError`）。
- `android_input_*` 依赖系统 `input`；`input text` 在部分 ROM 上字面量 `%s` 会被解释为空格。
- `android_toggle_setting` 依 ROM 差异可能部分项失败（返回 `applied=false` 与候选命令 stderr），
  **请以返回的 `state`（写入后读回）为准**。
- `android_screenshot` 为任务书逐字指定的两段式名称（无 `_{resource}`），
  作为**有据可查的命名例外**保留；其余 20 个规范工具均为严格三段式。
- WebUI 需新版 KernelSU Manager 支持 WebUI 特性；普通浏览器打开为**只读降级模式**。
- 隧道单设备单连接：同设备新连接会顶替旧连接。
- 流式转发协议（长流 / 大响应分帧）需要服务端 ≥ v1.1.0；旧版服务端降级为单帧转发。

### 本版本未在真机上验证的部分（开发环境无 Android 设备，如实标注）

- KernelSU Manager 内嵌 WebView 的**实际滚动帧率**；
- `file://` 页面到 `http://127.0.0.1` 的**跨源 `fetch` 可用性**
  （不可用时自动回落 `ksu.exec 'mcpd ui-state'`，功能完整、仅性能略降）；
- arm64 / arm 二进制在真机上的 **SELinux 通过性**；
- 真实 **WiFi↔4G 切换、飞行模式恢复**的时延；
- `screencap` / `input` / `svc` 在各厂商 ROM 上的可用性差异。

---

## 五、构建产物

| 文件 | 说明 |
|---|---|
| `ksu-mcp-server-v1.2.0.zip` | **KernelSU / Magisk 模块包**（5.7 MB，含 arm64 + arm 双架构二进制、安装脚本、WebUI） |
| `mcpd-arm64-v1.2.0` | 独立 arm64 二进制（aarch64，静态链接，已剥离符号） |
| `mcpd-arm-v1.2.0` | 独立 arm 二进制（ARM EABI5，静态链接，已剥离符号） |

---

## 六、验证情况

| 测试 | 用例数 | 结果 |
|---|---|---|
| Go 单元测试（`cd src && go test ./...`） | 24 个测试函数 | 全部通过 |
| MCP 协议端到端（`python3 tests/e2e_test.py`） | 87 项断言 | 全部通过 |
| 隧道端到端（`python3 tests/tunnel_e2e_test.py`） | 38 项断言 | 全部通过 |
| WebUI jsdom 冒烟 + 性能约定回归（`node tests/webui_test.js`） | 68 项断言 | 全部通过 |
| 稳定性长跑（`python3 tests/soak_test.py 7200 10`） | 2 小时 / 10s 采样 | **未在 v1.2.0 执行**；v1.2.1 阶段执行了 637 秒（0 断连 / 0 调用失败），完整 2 小时待后续执行 |

分析与诊断全文见仓库 `docs/analysis-and-diagnosis.md`（含 v1.1.0 六大问题的逐条根因定位到文件:行号）。
