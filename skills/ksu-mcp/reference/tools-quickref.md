# 工具速查（40 个：29 规范命名 + 11 弃用别名）

> 完整参数与返回字段以 MCP `tools/list` 或 `GET /api/tools` 为准；这里只给「选型要点」。

## A. 屏幕控件树（v1.3.0 新增，界面识别的首选）

| 工具 | 用途 | 关键参数 | 开销 |
|---|---|---|---|
| `android_get_screen_elements` | **结构化屏幕识别**：返回元素文字/id/类名/中心坐标 | `only_interactive`(默认 true)、`filter`、`max_elements`、`include_bounds`、`fresh` | 低（1~3KB） |
| `android_find_element` | 按选择器精确查找，看是否存在/有几个 | `text`/`text_contains`/`id`/`id_contains`/`desc`/`class_contains`/`editable`/`clickable`/`enabled`/`checked`/`password`/`ref`、`limit` | 极低（命中缓存） |
| `android_tap_element` | **按选择器一次点击**（自动取中心） | 同上选择器 + `match_index`、`wait_ms`、`verify` | 低 |
| `android_set_element_text` | **按选择器写入文本**（聚焦+清空+校验） | 选择器 + `content`(必填)、`clear_first`、`submit`、`verify` | 低 |
| `android_wait_for_element` | 等控件出现/消失 | 选择器 + `state`(present/absent)、`timeout_ms`、`interval_ms` | 低（设备端轮询） |
| `android_scroll_to_element` | 滚动查找控件 | 选择器 + `direction`、`max_scrolls`、`distance_percent`、`settle_ms` | 中 |
| `android_dump_ui_hierarchy` | 完整控件树（疑难排查） | `format`(json/xml)、`max_nodes`、`fresh` | **高**（10~80KB） |
| `android_get_foreground_app` | 前台应用包名与 Activity | 无 | 极低 |

**要点**：`get_screen_elements` 的 `ref` 只在同一次屏幕状态下有效；`wait`/`scroll` 不支持 `ref`。
控件树有 2 秒缓存，连续调用会自动复用。

## B. 只读信息采集

| 工具 | 用途 | 省 token 的调法 |
|---|---|---|
| `android_get_device_info` | 型号/Android/SDK/内核/root 方案与版本 | 无参数，直接调 |
| `android_get_battery_status` | 电量/温度/电压/充电器 | 无 |
| `android_get_storage_info` | 分区与关键路径容量 | 用 `path_filter="/data"` 只看关心的分区 |
| `android_get_network_info` | 网络类型/IP/网关/DNS/WiFi/运营商 | 无 |
| `android_list_packages` | 应用包名列表 | **必带** `filter` 或 `state`，`limit` 分页 |
| `android_get_running_processes` | 进程列表 | 带 `filter`，`sort_by="mem"`，`limit`≤30 |
| `android_get_logcat` | 日志快照 | **必带** `tags`/`filter`，`lines`≤200 |
| `android_get_prop` | 系统属性 | 指定 `key`；无参返回数百条 |
| `android_get_setting` | 读设置当前值 | 与 `toggle_setting` 同名 |
| `android_get_clipboard` | 读剪贴板 | 无 |

## C. 界面输入（会改变设备状态）

| 工具 | 用途 | 何时用 |
|---|---|---|
| `android_input_text` | 往**当前焦点**输入文本 | 已确认焦点时；否则优先 `android_set_element_text` |
| `android_input_key` | 按键（HOME/BACK/ENTER/音量/电源） | 返回上一级、回车、唤醒 |
| `android_input_tap` | 按坐标点击 | 只在控件树拿不到目标时（配合 `android_screenshot`） |
| `android_input_swipe` | 滑动手势 | 已知坐标的滚动/手势；列表找元素优先 `android_scroll_to_element` |
| `android_set_clipboard` | 写剪贴板 | 需要粘贴长文本时 |

## D. 设置、文件与命令

| 工具 | 用途 | 注意 |
|---|---|---|
| `android_toggle_setting` | 13 类开关/亮度/音量/勿扰 | **优先于 shell 拼命令**：多候选命令+读回校验 |
| `android_exec_shell` | root 执行 shell | 最强也最危险；受 `read_only`/`allowlist` 限制；批量查询请合并成一条命令 |
| `android_read_file` | 读文件 | 二进制自动 base64；先小 `max_bytes` |
| `android_write_file` | 写/追加文件 | `read_only` 下禁用；覆盖不可恢复 |
| `android_list_dir` | 列目录 | 只列一层 |

**`android_toggle_setting` 支持的设置名**：
布尔 `wifi` / `bluetooth` / `airplane_mode` / `mobile_data` / `location` / `auto_rotate` / `stay_awake`；
整数 `brightness`(0-255)、`volume_music`/`volume_ring`/`volume_alarm`/`volume_notification`(0-15)；
枚举 `dnd`（on/off/priority/total_silence/alarms_only）。

## E. 截图（最贵，谨慎）

| 工具 | 用途 |
|---|---|
| `android_screenshot` | 返回 MCP **image** 内容块；仅在需要看像素/图片/渲染时使用 |

> 做界面识别与操作时**不要**用它——用 A 组控件树工具。

## F. 弃用别名（仍可用，新代码请迁移）

| 旧名 | → 新名 |
|---|---|
| `device_info` | `android_get_device_info` |
| `battery_info` | `android_get_battery_status` |
| `network_info` | `android_get_network_info` |
| `app_list` | `android_list_packages` |
| `screenshot` | `android_screenshot` |
| `clipboard_get` | `android_get_clipboard` |
| `getprop` | `android_get_prop` |
| `exec_command` | `android_exec_shell` |
| `read_file` | `android_read_file` |
| `write_file` | `android_write_file` |
| `list_dir` | `android_list_dir` |
