# 端到端示例（含每步成本理由）

两个完整走查，展示「为什么这样调最划算」。

---

## 示例 A：给微信好友发一条消息

**任务**：「给微信里的『张三』发一句『我到家了』」

### 朴素做法（费时费 token，容易点错）
```
android_screenshot()                       # 一张图 ≈ 上千 token
# AI 目测微信图标坐标 → android_input_tap(x=..., y=...)   ← 估坐标，可能点偏
android_screenshot()                       # 再一张图确认打开了吗
# 目测搜索框 → tap → android_input_text("张三")
android_screenshot()                       # 又一张图
... 约 6~8 张截图 + 十几次调用
```

### 推荐做法（4~6 次调用，无截图）
```
# 1) 打开微信（monkey 不需要知道 Activity 名）
android_exec_shell(command="monkey -p com.tencent.mm -c android.intent.category.LAUNCHER 1")

# 2) 等首页就绪 —— 在设备端轮询，不消耗上下文
android_wait_for_element(text_contains="微信", timeout_ms=12000)

# 3) 一次拿到屏幕语义：哪里有搜索框、哪里有联系人
android_get_screen_elements()
#   → 看到 {"id":"com.tencent.mm:id/xxx","label":"搜索","editable":true,"center":[540,220]}
#           {"label":"张三","clickable":true,"center":[300,600]}

# 4) 若「张三」不在当前屏（会话列表很长），滚动找它
android_scroll_to_element(text_contains="张三", direction="down", max_scrolls=5)
#   → found=true，直接进入第 6 步；返回的 element.center 也可用

# 5) 或直接一次点击（控件树已缓存在 2 秒内，不会重复 dump）
android_tap_element(text="张三")           # → changed:true 表示进入会话

# 6) 等输入框出现后写入并发送
android_wait_for_element(class_contains="EditText", timeout_ms=5000)
android_set_element_text(class_contains="EditText", content="我到家了", verify=true)
android_tap_element(text="发送")           # 或 android_input_key(keycode="KEYCODE_ENTER")
```

**为什么省**：
- 全程 **0 张截图**，省下数千 token；
- 第 3 步的 dump 在第 4/5 步被缓存复用，只花一次 0.3~1.5s；
- 点击靠 `label`/`text` 语义定位，不用目测坐标，**不会点偏**；
- 等待交给 `android_wait_for_element`，AI 不必反复「sleep + 截图」。

**边界情况**：
- 微信是自绘程度较高的 App，若 `android_get_screen_elements` 返回的元素明显不足，
  可 `android_dump_ui_hierarchy(format="json", max_nodes=200)` 看层级；
  仍拿不到就退到「截图 + 坐标」，但**点完必须复核**。

---

## 示例 B：设备状态巡检 + 关掉 WiFi

**任务**：「看看手机电量、存储和网络，然后把 WiFi 关掉」

### 推荐做法（2 次调用完成巡检，1 次完成设置）
```
# 1) 四项信息用一条 shell 命令一次拿完（一次往返，而不是四次调用）
android_exec_shell(command="dumpsys battery | grep -E 'level|temperature'; df -h /data | tail -1; dumpsys wifi | grep -m1 'Wi-Fi is'")

# 2) 需要结构化时再分别调（按需，不是默认）
android_get_battery_status()
android_get_storage_info(path_filter="/data")

# 3) 关 WiFi —— 用专用工具，不要 shell 手拼命令
android_toggle_setting(setting="wifi", value=false)
#   → 返回 applied:true + state.value:false（工具已读回校验，可信）

# 4) 确认
android_get_setting(setting="wifi")
```

**为什么省 / 为什么稳**：
- 巡检用**一条组合命令**：4 项信息 1 次调用，而不是 4 次；
- 关 WiFi 用 `android_toggle_setting` 而非 `svc wifi disable`：
  不同 ROM 支持的命令不同，该工具会按候选顺序尝试**并读回真实状态**，
  返回值里的 `state` 才是可信结果；
- `path_filter` 只看 `/data`，避免把十几个分区全塞进上下文。

> 注意：关掉 WiFi 可能让**局域网/公网隧道的 MCP 连接一起断开**。
> 若是通过 WiFi 局域网连接的，改用移动数据或先告知用户。

---

## 反例清单（这些写法要避免）

| 反例 | 为什么错 | 改成 |
|---|---|---|
| 用 `android_screenshot` 找按钮 | 费 token 且要目测坐标 | `android_get_screen_elements` 或 `android_tap_element(text=...)` |
| `android_find_element` 后再 `android_input_tap(center)` | 多一次往返 | 直接 `android_tap_element(同名选择器)` |
| `android_get_screen_elements()` 后马上 `fresh=true` 再抓 | 白等 0.3~1.5s | 依赖 2 秒缓存，不传 `fresh` |
| sleep 3 秒再截图确认 | 慢且不可靠 | `android_wait_for_element` |
| `android_get_logcat()` 无参 | 输出巨大，撑爆上下文 | `tags=[...]`、`filter=...`、`lines=200` |
| `android_list_packages()` 无参 | 数百个包名 | `filter="tencent"` 或 `limit` 分页 |
| 用 `android_input_text` 直接打字 | 焦点未知，可能打错地方 | `android_set_element_text(选择器, content)` |
| 破坏性命令直接执行 | 不可逆 | 先向用户确认 |
