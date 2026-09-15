# KSU MCP Server v1.3.0

> 协议 **MCP 2025-11-25** · 工具 **40 个**（29 规范命名 + 11 弃用别名）· 新增 **屏幕控件树工具** 与 **AI 技能包**

**本版两个看点：**
1. **用结构化数据替代截图做界面识别** —— 新增 8 个控件树工具，直接给出元素文字、资源 id 与**精确中心坐标**，一次调用即可「找到并点击」，不再靠目测截图估坐标。**原有截图工具完全未改动。**
2. **配套 AI 技能包** —— 教 AI 怎么调用最省时、最省 token（成本模型、决策树、选择器排序、排障清单、端到端示例），已单独打包为 `ksu-mcp-skills-v1.3.0.zip`。

升级：KernelSU Manager → 模块 → ＋ → 选择 `ksu-mcp-server-v1.3.0.zip` 覆盖安装即可，配置自动保留。

---

## 一、新增：屏幕控件树工具（8 个）

### 为什么

`android_screenshot` 返回一张 PNG（MCP image 内容块），对 AI 有**两个结构性缺陷**：

- **贵**：一张 1080×2400 截图编码后上千 token，而任务常常只是「找到某按钮并点它」；
- **不可靠**：截图只给像素，AI 必须**目测估算**坐标，分辨率/缩放/状态栏一变就点偏，
  也无法判断控件是否可点、是否禁用、是否已勾选。

新工具直接读 Android `uiautomator` 的控件树 XML，返回**结构化元素**
（文本 / 资源 id / 类名 / 可见描述 / 精确中心坐标），1~3KB JSON，
并可**一次调用完成「查找 + 计算坐标 + 点击」**。

### 工具清单

| 工具 | 用途 |
|---|---|
| `android_get_screen_elements` | **界面识别首选**：返回屏幕上可点/可输入元素的文本、id、类名与 `center` 坐标；支持 `filter`、`max_elements`、`include_bounds`、`fresh` |
| `android_find_element` | 按选择器精确查找（文本/id/描述/类名/可点/可输入/已勾选/禁用/`ref`），返回匹配数与坐标 |
| `android_tap_element` | **按选择器一次点击**（自动取中心坐标），支持 `match_index`、`wait_ms`、`verify` 复核 |
| `android_set_element_text` | **按选择器写入文本**：自动聚焦 → 移到行尾 → 清空 → 输入 → **读回校验** |
| `android_wait_for_element` | 等待控件出现/消失（`state`=present/absent），在设备端轮询，AI 无需 sleep + 截图 |
| `android_scroll_to_element` | 沿 `direction` 反复滚动查找控件，直到找到或达 `max_scrolls` |
| `android_dump_ui_hierarchy` | 完整控件树（`format`=json 嵌套树 / xml 原文），输出 10~80KB，仅疑难场景使用 |
| `android_get_foreground_app` | 当前前台应用包名与 Activity（一次 dumpsys，最省） |

### 实现要点（这些细节决定了它好不好用）

- **`label` 由子孙文本合成**：可点击的列表项容器自身通常没有 `text`，
  工具会把子节点文本拼成 `label`（如 `"微信 版本 8.0.49"`），因此
  `android_tap_element(text_contains="微信")` 能直接点中容器。
- **冗余节点去重**：与祖先文本相同且自身不可交互的节点会被跳过，避免同一文字出现两次浪费 token。
- **布尔标记按需输出**：`clickable`/`editable`/`checked`/`focused` 等仅在 `true` 时出现，
  `enabled:false` 仅在禁用时出现 —— 显著压缩载荷。
- **2 秒内存缓存**：`android_get_screen_elements` → `android_find_element` → `android_tap_element(ref=)`
  只 dump 一次，省掉 0.3~1.5s。只有显式 `fresh=true` 才重新抓取。
- **抗动画干扰**：`uiautomator dump` 常因界面动画报 `could not get idle state`，
  已做 3 次重试 + `--compressed` 回退，并用「dump 文件 mtime 早于命令开始时间」判定陈旧结果。

### 原有截图工具保持不变

`android_screenshot` 与弃用别名 `screenshot` **一行未改**，行为与 v1.2.x 完全一致。
两者互补使用：
- **控件树** → 找控件、点按钮、填表单、读界面文字（默认）
- **截图** → 看图片/图表内容、渲染是否异常、控件树查不到目标时

---

## 二、新增：AI 技能包（`ksu-mcp-skills-v1.3.0.zip`）

工具多（40 个）不代表 AI 会用得好。技能包解决「**按什么顺序、用什么姿势调用最划算**」。

| 内容 | 说明 |
|---|---|
| **成本模型** | 40 个工具按「耗时 + 上下文开销」分 8 档排序，明确**界面识别优先控件树、而非截图** |
| **决策树** | 从「看屏幕」到「点击/输入/滚动/等待」的 8 条分支 |
| **五条硬规则** | 先控件树后截图 · 不要两者都调 · 不要拆成两步 · 不要 sleep 猜加载 · 不要凭坐标硬点 |
| **缓存复用** | 控件树 2 秒缓存的具体用法 |
| **参数纪律** | 日志要 tag/行数、包列表要分页、进程要过滤，避免上下文被撑爆 |
| **选择器稳定性排序** | `id` > `id_contains` > `desc` > `text` > `text_contains` > `class_contains` > 坐标 |
| **排障清单** | `could not get idle state`、点了没反应、`verified=false`、隧道 `device offline` 等 20+ 场景 |
| **端到端示例** | 发微信、状态巡检两个完整走查，含每步成本理由与反例清单 |

```
ksu-mcp/
├── SKILL.md                       # 主技能（成本模型 + 决策树 + 配方 + 安全边界）
├── skill.json                     # 机器可读清单（版本/依赖/触发词/各客户端安装路径）
├── reference/tools-quickref.md    # 40 个工具速查
├── reference/ui-recipes.md        # 界面自动化剧本 + 选择器排序
├── reference/troubleshooting.md   # 排障清单
└── examples/end-to-end.md         # 端到端示例 + 反例清单
```

**安装**：

```bash
curl -LO https://github.com/HZY-520/KSU-MCP/releases/download/v1.3.0/ksu-mcp-skills-v1.3.0.zip
unzip -o ksu-mcp-skills-v1.3.0.zip -d /tmp/ksu-skills
mkdir -p ~/.claude/skills && cp -r /tmp/ksu-skills/ksu-mcp ~/.claude/skills/   # Claude Code
mkdir -p ~/.dsh/skills    && cp -r /tmp/ksu-skills/ksu-mcp ~/.dsh/skills/      # DeepSeek Harness
```

> 更省事的方式：把仓库 README 里的**「一键安装提示词」**整段复制给你的 AI，它会自己装好技能并引导你装手机端模块。
> 不支持目录式技能的客户端：把 `SKILL.md` 全文粘进系统提示即可。

---

## 三、升级注意事项

1. **设备端与服务端均无需改动配置**：本次只新增工具与技能包，`config.json` 完全兼容。
2. **覆盖安装即可**：KernelSU Manager → 模块 → ＋ → `ksu-mcp-server-v1.3.0.zip`。
3. **技能包需要服务端 ≥ v1.3.0**（其中的控件树工具在旧服务端上不存在，技能会引导 AI 退回截图方案）；
   技能包版本与服务端版本同步发布。
4. **控件树工具依赖系统 `uiautomator`**：极少数精简 ROM 可能不可用，
   此时工具返回带原因的 `isError`，按技能中的指引退回截图方案即可。
5. 老客户端不受影响：11 个弃用别名（`device_info`、`screenshot` 等）继续可用。

## 四、已知限制

- `android_set_element_text` 的 `clear_first` 通过「移到行尾 + 按 DEL」实现，
  对极端自定义输入框可能清不干净；可用 `verified` 字段判断是否成功。
- `android_get_screen_elements` 的 `ref` 仅在**同一次屏幕状态**下有效，界面变化后需重新获取；
  `android_wait_for_element` / `android_scroll_to_element` 因此不支持 `ref`。
- 控件树反映的是**无障碍节点**，游戏与高度自绘界面可能只有少量节点，需退回截图 + 坐标方案。
- 与 v1.2.x 相同的限制继续适用（`screencap`/`input`/`svc` 依 ROM 差异、
  WebView 实际帧率、`file://` 跨源 fetch、ARM 二进制 SELinux 通过性、真实网络切换时延等），详见 v1.2.0 Release Notes。

## 五、验证情况

| 测试 | 用例数 | 结果 |
|---|---|---|
| Go 单元测试（`go test -race`） | 25 个测试函数 | 全部通过 |
| MCP 协议端到端（新增 42 项控件树断言） | **148 项**断言 | 全部通过 |
| 隧道端到端（真实 Node 服务端 + 真实 mcpd） | 47 项断言 | 全部通过 |
| WebUI jsdom 冒烟 + 性能约定回归 | 74 项断言 | 全部通过 |

控件树测试覆盖：`label` 子孙文本合成、冗余节点去重、`center` 坐标、
各选择器分支（文本/子串/id/描述/类名/editable/password/enabled/checked/scrollable）、
`tap_element` 实际下发坐标、`set_element_text` 的「聚焦→清行尾→清空→输入」完整序列与 `verified` 读回、
`wait`/`scroll` 语义与超时、前台应用解析，以及**「原截图工具未被改动」的回归断言**。
测试桩新增 `uiautomator` 与 `dumpsys window`，并让 `input` 桩把输入内容回写到 dump，
从而真实覆盖 `verify` 分支。

> **关于服务端包**：v1.2.0 / v1.2.1 两个 Release 中的服务端包都命名为 `...-v1.2.0.zip`，
> 但内容存在细微差异（v1.2.1 那个包含后续的丢包指标与管理台修复），容易产生歧义。
> 自 v1.3.0 起改为**包名与服务端 `VERSION`、Release 版本三者一致**，本版即 `ksu-mcp-tunnel-server-v1.3.0.zip`。

> 说明：以上均在 Linux 服务器上以 `tests/fakebin/` 模拟 Android 命令验证解析与协议行为；
> 真机相关项（具体 ROM 的 `uiautomator` 可用性、控件树节点完整度）需在设备上确认。

## 六、构建产物

| 文件 | 说明 |
|---|---|
| `ksu-mcp-server-v1.3.0.zip` | **KernelSU / Magisk 模块包**（含 arm64 + arm 双架构二进制、安装脚本、WebUI） |
| **`ksu-mcp-skills-v1.3.0.zip`** | **AI 技能包**（SKILL.md + skill.json + reference/ + examples/） |
| **`ksu-mcp-tunnel-server-v1.3.0.zip`** | **公网穿透服务端包**（server.js / admin.html / 配置样例 / Nginx 样例 / 部署手册；不含 config.json 与 node_modules）。服务端功能与 v1.2.1 相同（含丢包指标与管理台登录框修复），自本版起**包名与服务端 VERSION、Release 版本三者一致** |
| `mcpd-arm64-v1.3.0` | 独立 arm64 二进制（aarch64，静态链接，已剥离符号） |
| `mcpd-arm-v1.3.0` | 独立 arm 二进制（ARM EABI5，静态链接，已剥离符号） |
