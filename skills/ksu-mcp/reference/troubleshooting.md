# 排障清单

按「现象 → 可能原因 → 正确处置」组织。先只读侦察，再动设备。

---

## 一、连不上（客户端侧）

| 现象 | 原因 | 处置 |
|---|---|---|
| `initialize` 无响应 / 连接超时 | 服务未运行 | 设备端 `mcpd status` 看 `running`；不行则 `mcpd start` |
| HTTP 401 / `unauthorized` | Token 不对 | 设备端 `mcpd token` 或 WebUI「接入信息」查看；公网用服务端 `clientToken` |
| HTTP 404 + `device not found`（公网） | 设备名与服务端登记不一致 | 核对 URL 里的 `<设备名>` 与服务端管理台 `/admin` 的设备列表 |
| 公网 502 + `device offline` | 手机侧隧道断连 | 设备端 `mcpd tunnel-status` 看 `state`；`connected=true` 才算通 |
| 局域网连不上 | 未开启局域网访问 | WebUI「运行配置 → 局域网访问」打开并保存（监听 `0.0.0.0`） |
| 端口连不上但服务在跑 | 端口改了 | `mcpd status` 看实际 `port` |

## 二、工具调用失败

| 现象 | 原因 | 处置 |
|---|---|---|
| `服务器处于只读模式（read_only=true）` | 只读闸门 | 改用只读工具；确需写操作请用户去 WebUI 关闭只读 |
| `命令不在允许列表内` | `exec_allowlist` 生效 | 换白名单内命令，或请用户 `mcpd config-set --allowlist-add "<cmd>"` |
| `unknown tool: xxx` | 工具名写错/版本旧 | `mcpd tools` 查实际可用工具；旧名已弃用，改用 `android_*` |
| `setting 不能为空，可选值: …` | 设置名不在白名单 | 用返回的可选值之一 |
| `value 超出范围` | 亮度/音量越界 | brightness 0-255；音量 0-15 |

## 三、界面类工具（v1.3.0 控件树）

| 现象 | 原因 | 处置 |
|---|---|---|
| `uiautomator dump 失败…could not get idle state` | 界面有动画/正在刷新 | 工具内部已重试 3 次；仍失败先 `android_wait_for_element` 等稳定，或先停止动画（开发者选项「动画时长 0.5x」） |
| `uiautomator dump 失败` / 报工具不存在 | 个别 ROM 精简了 uiautomator | 退回 `android_screenshot` + 目测坐标 + `android_input_tap` |
| `uiautomator dump 结果陈旧` | 极少数 ROM 不刷新文件 | 重试；仍不行改用截图方案 |
| `未找到匹配控件` | 选择器写法与控件不符 / 界面已变 | 先 `android_get_screen_elements` 看真实 label/id；注意 `label` 可能是子孙文本拼的 |
| `ref 与 wait_ms 不能同时使用` | 语义冲突 | 等待场景改用文本/id 选择器（ref 会随界面失效） |
| 找到元素但 `changed=false` | 没点中（被遮挡/真实目标在父容器） | 看该 `center` 归属；改点外层容器，或用 `android_find_element` 看清层级 |
| `verified=false` | 输入未真正聚焦 | 先 `android_tap_element` 点输入框，再 `android_set_element_text` |
| `滚动到上限仍未找到` | 已到底 / 目标在别的标签页 | 检查是否需要先切标签、展开分组；或换 `direction` |
| 返回元素太多 | 默认只返回可交互项，仍多 | 加 `filter`，或 `max_elements=30`、`include_bounds=false` |

## 四、性能与成本

| 现象 | 原因 | 处置 |
|---|---|---|
| 每步都很慢 | 每步重新 dump（0.3~1.5s） | 连续操作时**不要**传 `fresh=true`，复用 2 秒缓存 |
| 上下文被撑爆 | 用了 `android_dump_ui_hierarchy` 或大 `lines`/无 `limit` | 改用 `android_get_screen_elements`；大输出工具加过滤与分页 |
| token 花得比预期多 | 用了 `android_screenshot` 做界面识别 | 换成控件树工具；截图只在需要视觉时用 |
| AI 反复 sleep + 截图 | 用错了等待方式 | 用 `android_wait_for_element`（设备端轮询，不占上下文） |

## 五、设备状态类

| 现象 | 处置 |
|---|---|
| `android_screenshot` 失败 | 系统 `screencap` 不可用；尝试 `android_exec_shell(command="screencap -p /data/local/tmp/s.png")` 后 `android_read_file(encoding="base64")` |
| 电池/存储字段为 null | 该 ROM 无对应 sysfs 节点；看返回的 `source` 字段判断数据来源 |
| `dumpsys` 相关工具返回 null | ROM 输出格式不同；用 `android_exec_shell(command="dumpsys window \| head -40")` 看原始输出 |
| WiFi SSID 为 null | Android 高版本权限限制；`android_exec_shell(command="cmd wifi status")` 试试 |
| 命令超时 | `android_exec_shell` 默认超时 30s；长任务显式给 `timeout`（≤3600） |

## 六、动手前的检查清单

- [ ] 已确认前台应用（`android_get_foreground_app`）是目标 App
- [ ] 破坏性命令（删除/格式化/改系统设置）已获用户确认
- [ ] 大输出工具已加过滤/分页
- [ ] 界面操作优先用了控件树，而不是截图
- [ ] 操作后用 `changed` / `verified` / `android_get_foreground_app` 复核了结果
