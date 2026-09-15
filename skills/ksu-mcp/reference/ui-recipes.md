# 界面自动化深入剧本（控件树优先）

核心思想：**用控件树拿到「语义 + 精确坐标」，不要用像素去猜。**

---

## 一、为什么控件树优于截图

| 维度 | 控件树（`android_get_screen_elements`） | 截图（`android_screenshot`） |
|---|---|---|
| 输出 | 1~3KB JSON | 一张图（上千 token） |
| 能否直接知道「这是什么」 | 能：文本 / 资源 id / content-desc / 类名 | 不能，需视觉模型推断 |
| 能否直接知道「点哪里」 | 能：每个元素带 `center` | 不能，需目测估算 |
| 是否受分辨率/缩放影响 | 不受 | 受 |
| 能否判断可点/禁用/勾选/焦点 | 能（布尔标记） | 很难 |
| 何时才该用 | **默认** | 需要看图片内容、图表、渲染异常、或控件树查不到目标 |

---

## 二、元素怎么被识别（理解这点才能写出稳的选择器）

1. **`label` 是「可见语义」**：元素自身没有 `text` 时，工具会把**子孙文本**拼成 `label`。
   所以列表项那种「一个可点击容器 + 里面几个 TextView」的结构，容器也会带 `label="微信 版本 8.0.49"`，
   你可以直接 `android_tap_element(text_contains="微信")` 点中容器。
2. **冗余节点已去重**：文本与祖先相同的子节点会被跳过（否则同一个「设置」会出现两次）。
3. **坐标是像素 `center`**：直接可喂给 `android_input_tap`。
4. **`ref` 只在同一次 dump 内有效**：界面一变就作废。跨步骤操作请用**文本/id 选择器**，不要存 ref。

### 选择器选择顺序（稳 → 脆）

```
1. id（资源 id）            android_tap_element(id="com.x:id/btn_ok")            ← 最稳
2. id 子串                  android_tap_element(id_contains="btn_ok")           ← 宿主 id 前缀会变时用
3. desc（无障碍描述）        android_tap_element(desc="返回")                     ← 图标按钮常用
4. text（精确文字）          android_tap_element(text="登录")                     ← 直观但易受多语言影响
5. text_contains（子串）     android_tap_element(text_contains="登")              ← 文字会变时用
6. class_contains + 属性     android_find_element(class_contains="EditText")      ← 找不到标识时的兜底
7. 坐标（最后手段）          android_input_tap(x=, y=)                            ← 必须配截图+复核
```

---

## 三、七种典型场景

### 场景 1：确认当前在哪
```
android_get_foreground_app()
```
最便宜。操作前后各调一次，就能判断「是否跳转成功」。

### 场景 2：看看屏幕上有什么
```
android_get_screen_elements()
```
元素太多时：
```
android_get_screen_elements(filter="登录", include_bounds=false)
```
> `include_bounds=false` 在「只想确认存在性」时省一半输出。

### 场景 3：点一个可见按钮
```
android_tap_element(text="登录")
```
名称会变时：
```
android_tap_element(id_contains="login")            # 或
android_tap_element(text_contains="登")
```
点完看返回的 `changed`：`true` 表示界面确实变了。

### 场景 4：填输入框
```
# 已知 id（最稳）
android_set_element_text(id="com.x:id/phone", content="13800000000")

# 不知道 id：按可见提示文字定位
android_set_element_text(text_contains="手机号", content="13800000000")
```
- `clear_first` 默认 true，会先移到行尾再按 DEL 清空。
- `submit=true` 会在输入后按回车（搜索框常用）。
- `verified=true` 表示读回的文本与目标一致 → 真的写进去了。

### 场景 5：等界面就绪（不要 sleep）
```
# 打开应用后等首页标志出现
android_exec_shell(command="monkey -p com.tencent.mm -c android.intent.category.LAUNCHER 1")
android_wait_for_element(text_contains="微信", timeout_ms=10000)

# 等一个加载框消失
android_wait_for_element(text_contains="加载中", state="absent", timeout_ms=15000)
```

### 场景 6：长列表里找东西
```
android_scroll_to_element(text_contains="开发者选项", direction="down", max_scrolls=8)
```
- `direction` 指的是**内容滚动方向**：`down` = 看下面的内容（手指上滑）。
- 找到后返回 `element.center`，大多数情况下直接跟一个 `android_tap_element` 同名选择器即可。
- `found=false` 说明已到底或目标不在本页（换标签页/需要先展开分组）。

### 场景 7：自绘控件 / 游戏 / 控件树拿不到
```
android_screenshot()                       # 目测目标像素坐标
android_input_tap(x=540, y=1200)           # 点它
android_get_screen_elements()              # 必须复核（或 android_get_foreground_app）
```
> 这是唯一合理的「截图 + 坐标」用法。

---

## 四、稳定性技巧

| 问题 | 做法 |
|---|---|
| 点击无反应 | 目标可能被遮挡：`android_get_screen_elements` 看该 `center` 是否落在另一个元素里；改点外层容器 |
| 有多个同名元素 | 用 `match_index`（0 起，按屏幕从上到下）；或先 `android_find_element` 看清有几个 |
| 文字随语言/状态变化 | 优先 `id` 或 `id_contains` |
| 输入后没生效 | 先 `android_tap_element` 点输入框，再 `android_set_element_text`；检查返回的 `verified` |
| dump 报 `could not get idle state` | 界面在动；工具已自动重试 3 次，仍失败就先 `android_wait_for_element` 等稳定，或直接用截图 |
| 弹窗/权限框挡住操作 | 先 `android_get_screen_elements` 找弹窗按钮（常有 `text="允许"`），点掉再继续 |
| 键盘挡住下方元素 | 先 `android_input_key(keycode="KEYCODE_BACK")` 收起键盘，或在 `set_element_text` 里 `submit=true` 提交后消失 |
