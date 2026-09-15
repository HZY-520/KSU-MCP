---
name: ksu-mcp
description: 通过 KSU MCP Server 操控已 root 的 Android 设备。当用户说「操作我的手机 / 看看手机屏幕 / 点一下某个按钮 / 在手机上输入 / 发条微信 / 查手机电量、存储、网络、进程、日志 / 装了什么应用」等涉及真实 Android 设备的任务时使用。本技能内置成本模型与调用配方，指导 AI 用最少 token、最短耗时、最稳路径完成任务（优先控件树结构化识别，而非截图）。
---

# KSU MCP 使用技能

本技能教你**怎么用** KSU MCP 这套工具最高效地完成任务。工具本身的能力描述在 MCP 的
`tools/list` 里，这里只讲**选择、顺序与省钱省时的门道**。

第 0 步永远先做：如果还没接入，见本文件末尾「接入检查」。

---

## 一、成本模型（决定你怎么选工具）

按「耗时 + 上下文开销」从低到高排序。**能用上面的就不要用下面的。**

| 档位 | 工具 | 实测开销 | 什么时候用 |
|---|---|---|---|
| ① 极轻 | `android_get_foreground_app` | 一次 dumpsys，输出 ~120 字节 | 确认「现在在哪个 App / 哪个页面」；操作后确认是否跳转成功 |
| ② 轻（**界面识别默认选它**） | `android_get_screen_elements` | 0.3~1.5s dump，输出 1~3KB | 需要知道屏幕上有哪些可点/可输入的元素、它们的文字与坐标 |
| ③ 轻（复用②的缓存） | `android_find_element` | 命中 2s 内缓存则 ≈0ms | 只想确认某元素是否存在 / 有几个候选 |
| ④ **一次做完（最省往返）** | `android_tap_element`、`android_set_element_text`、`android_wait_for_element`、`android_scroll_to_element` | 内部含 dump，但**一次调用完成「找 + 做」** | 知道要干什么时的默认选择 |
| ⑤ 中 | `android_get_battery_status` / `_storage_info` / `_network_info` / `_setting` / `android_get_prop` | 数十~数百毫秒 | 查设备状态 |
| ⑥ 中偏大 | `android_list_packages` / `android_get_running_processes` / `android_get_logcat` | 输出可能很大 | **必须**加过滤与分页参数（见下） |
| ⑦ 重 | `android_dump_ui_hierarchy` | 输出 10~80KB | ②解释不了的疑难（自绘控件、无文本无 id） |
| ⑧ **最贵** | `android_screenshot` | 一张图 = 上千 token，且要你自己估坐标 | 只有需要**看像素**时才用（图片内容、图表、渲染异常、控件树查不到目标） |

### 五条硬规则

1. **先控件树，后截图。** 需要「找到并操作某元素」时，`android_screenshot` 几乎总是错的答案：
   它给你像素，你还得目测坐标，既费 token 又易点错。用 `android_get_screen_elements` 直接拿
   文本 + 精确 `center`，或干脆 `android_tap_element(text="登录")` 一步到位。
2. **不要既截图又抓控件树。** 二选一；需要视觉时才截图，需要操作时用控件树。
3. **别拆成两步。** 有 `android_tap_element` 就不要写 `android_find_element` → `android_input_tap`；
   有 `android_set_element_text` 就不要写 `点击输入框` → `android_input_text`。
4. **别用 sleep 猜加载。** 用 `android_wait_for_element`（在设备端轮询，不消耗你的上下文）。
5. **别凭坐标硬点。** 只有控件树里确实拿不到目标（自绘/游戏画面）时，才用
   `android_screenshot` + `android_input_tap`，且点完必须用 `android_get_screen_elements` 或
   `android_get_foreground_app` 复核。

### 缓存复用（重要）

控件树有 **2 秒 TTL 内存缓存**。所以下面这组写法只 dump 一次：

```
android_get_screen_elements()          # 抓一次 dump
android_find_element(text="确定")       # 复用缓存，≈0ms
android_tap_element(ref=<上面返回的 ref>) # 复用缓存
```

**不要在 `android_get_screen_elements` 之后立刻传 `fresh=true`** —— 那是白花一次 0.3~1.5s。
只有当你怀疑界面已变化时才 `fresh=true`。

### 大输出工具的参数纪律

| 工具 | 必须这样调 |
|---|---|
| `android_list_packages` | 带 `filter` 或 `state`，并用 `limit`（默认 100）分页；翻页用 `offset += count` |
| `android_get_logcat` | 带 `tags` 或 `filter`，`lines` ≤ 200；不要无参拉整段日志 |
| `android_get_screen_elements` | 元素多时先 `filter`，或把 `max_elements` 压到 30~50、`include_bounds=false` |
| `android_get_running_processes` | 带 `filter`/`sort_by`，`limit` ≤ 30 |
| `android_read_file` | 先小 `max_bytes`（如 4096）探明格式，再按需放大 |
| `android_exec_shell` | 多个查询用**一条命令**组合（`cmd1; cmd2; cmd3`），不要分多次调用 |

---

## 二、决策树（照着走就不会错）

```
用户要在手机上做一件事
│
├─ 只想知道「现在在哪个 App/页面」？ → android_get_foreground_app
│
├─ 需要看到屏幕上有哪些可操作元素？ → android_get_screen_elements
│    └─ 想缩小范围就加 filter="关键词"；只关心坐标就 include_bounds=false
│
├─ 要点某个「有文字/有 id」的东西？
│    → android_tap_element(text="登录")          ← 首选，一次调用
│    → android_tap_element(id_contains="btn_ok")  ← 文字会变时更稳
│
├─ 要往输入框写字？
│    → android_set_element_text(id="xxx", content="内容")   ← 自动聚焦+清空+校验
│    → 若不知道 id：android_set_element_text(text_contains="搜索", content="...")
│
├─ 目标不在当前屏（长列表/需要下滑）？
│    → android_scroll_to_element(text="设置", direction="down", max_scrolls=5)
│
├─ 界面还没加载好？
│    → android_wait_for_element(text_contains="首页", timeout_ms=8000)
│
├─ 元素没有文字也没有 id（自绘控件）？
│    → android_dump_ui_hierarchy(format="json") 看层级与 bounds
│    → 仍不行 → android_screenshot 目测 → android_input_tap(x=,y=) → 复核
│
└─ 需要视觉判断（图片/图表/渲染是否正常）？
     → android_screenshot（此时它才是对的工具）
```

**点完必须复核**（`verify` 默认就是开的，别关）：`android_tap_element` 返回
`changed: true` 说明界面确实变了；`false` 说明可能没点中（被遮挡/坐标偏移）。

---

## 三、常用配方（复制即用）

### 3.1 打开应用并等它加载好

```
android_exec_shell(command="monkey -p com.tencent.mm -c android.intent.category.LAUNCHER 1")
android_wait_for_element(text_contains="微信", timeout_ms=10000)
```

> 比 `am start` 更省事的地方：`monkey` 不需要你知道 Activity 名。已知 Activity 时用
> `am start -n 包名/Activity` 更精准。

### 3.2 在列表里找到并点某一项（长列表）

```
android_scroll_to_element(text_contains="设置", direction="down", max_scrolls=6)
# 返回 found=true 且带 element.center → 直接
android_tap_element(text_contains="设置")
```

### 3.3 填表单并提交

```
android_set_element_text(id="com.x:id/phone", content="13800000000")
android_set_element_text(id="com.x:id/pwd", content="secret", submit=false)
android_tap_element(text="登录")
android_wait_for_element(text_contains="首页", timeout_ms=8000)
```

### 3.4 读取界面上的所有可见文字（不做操作，纯信息提取）

```
android_get_screen_elements(only_interactive=false, max_elements=60, include_bounds=false)
```
> 比截图 + OCR 又快又准，且不需要视觉模型。

### 3.5 批量设备状态巡检（一条命令搞定）

```
android_exec_shell(command="getprop ro.product.model; cat /proc/loadavg; df -h /data | tail -1; dumpsys battery | grep level")
```
> 一次往返拿四项信息。**不要**分四次调用。

### 3.6 改系统开关（不要用 shell 手拼命令）

```
android_toggle_setting(setting="wifi", value=false)      # 会自动读回校验
android_get_setting(setting="brightness")                 # 改前/改后确认
```

### 3.7 排障：应用崩了

```
android_get_foreground_app()
android_get_logcat(tags=["AndroidRuntime"], priority="E", lines=80)
```

---

## 四、安全与失败处理

**动手前先判断闸门**（在设备端 `config.json` 配置，AI 无法绕过）：

| 闸门 | 表现 | 你要怎么做 |
|---|---|---|
| `read_only=true` | `android_exec_shell` / `android_write_file` 返回 `isError` | 改用只读工具；需要写操作就告知用户去 WebUI 关闭只读 |
| `exec_allowlist` 非空 | 命令返回「命令不在允许列表内」 | 换成允许的命令；或告知用户追加白名单 |
| Token 错误 | 连接被拒（401） | 让用户用 `mcpd token` 或 WebUI 查看 Token |

**常见错误与正确反应**：

| 现象 | 原因 | 处理 |
|---|---|---|
| `could not get idle state` | 界面有动画/正在刷新 | 直接重试（工具内部已重试 3 次）；仍失败先 `android_wait_for_element` 等界面稳定 |
| `uiautomator dump 失败` / 工具不可用 | 个别 ROM 无 uiautomator | 退回 `android_screenshot` + 目测坐标 |
| `device offline`（经公网隧道时） | 手机侧隧道断连 | 查设备端 `mcpd tunnel-status`；`state=connected` 才算通 |
| 找到元素但点了没反应 | 目标被上层遮挡 / 真实点击目标在父容器 | 用 `android_get_screen_elements` 看该 center 归属哪个元素；改点父容器或 `ref` |
| `verified: false`（输入后读回不符） | 未真正聚焦到该输入框 | 先 `android_tap_element` 点一下该框，再 `android_set_element_text` |

**红线**：`android_exec_shell` 能 `rm -rf`、能改系统分区。破坏性命令（删除、格式化、改系统设置）执行前
必须先向用户确认；不确定的先用只读方式探明现状。

---

## 五、完整工具索引

按用途分组见 `reference/tools-quickref.md`；界面自动化深入剧本见 `reference/ui-recipes.md`；
错误处理清单见 `reference/troubleshooting.md`。

---

## 六、接入检查（第 0 步）

1. **服务端是否可达**：设备本机 `http://127.0.0.1:9123/mcp`；局域网 `http://<设备IP>:9123/mcp`；
   公网 `https://<隧道域名>/mcp/<设备名>`。
2. **鉴权**：请求头 `Authorization: Bearer <Token>`（本机/局域网用设备 Token，
   公网用隧道服务端下发的 `clientToken`）。
3. **首次连接顺序**：`initialize`（协议 2025-11-25）→ `notifications/initialized` → `tools/list`。
4. **自检三连**（确认链路与权限）：
   ```
   android_get_device_info()      # 型号/Android 版本/root 状态
   android_get_foreground_app()   # 前台应用
   android_exec_shell(command="echo ok")   # 若 read_only 会报错，属预期
   ```
5. 工具名有 `android_` 前缀的为规范命名；旧的扁平名（`device_info`、`screenshot` 等）
   仍可用但已弃用，新代码请用规范名。
