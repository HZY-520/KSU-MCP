# KSU MCP Skills

一套给 AI Agent 用的技能包，教它**怎么调用 KSU MCP 这套工具最省时、最省 token、最稳**。

配套的 MCP 服务端在本仓库的 `module/`（手机端 KernelSU 模块）与 `tunnel-server/`（公网穿透服务端）。
技能包解决的是另一个问题：**工具很多（40 个），AI 该按什么顺序、用什么姿势调。**

---

## 包含内容

```
skills/
├── README.md                       # 本文件
└── ksu-mcp/                        # ← 要安装的技能（整个目录）
    ├── SKILL.md                    # 主技能：成本模型 + 决策树 + 配方 + 安全边界
    ├── skill.json                  # 机器可读清单（名称/版本/依赖/触发词/各客户端安装方式）
    ├── reference/
    │   ├── tools-quickref.md       # 40 个工具速查（含省 token 的调法）
    │   ├── ui-recipes.md           # 界面自动化剧本 + 选择器稳定性排序
    │   └── troubleshooting.md      # 20+ 场景的排障清单
    └── examples/
        └── end-to-end.md           # 端到端示例（含每步成本理由）+ 反例清单
```

## 它到底教了 AI 什么

1. **成本模型**：把 40 个工具按「耗时 + 上下文开销」分成 8 档并排序，明确
   **界面识别优先用控件树（`android_get_screen_elements`），而不是截图**——截图一张就是上千 token，
   还得靠目测估坐标，既贵又容易点错。
2. **决策树**：从「看屏幕」到「点击 / 输入 / 滚动 / 等待」的 8 条分支，照着走不会选错工具。
3. **五条硬规则**：先控件树后截图、不要两者都调、不要拆成两步、不要 sleep 猜加载、不要凭坐标硬点。
4. **缓存复用技巧**：控件树有 2 秒缓存，`get_screen_elements` → `find_element` → `tap_element(ref=)`
   只 dump 一次，省 0.3~1.5 秒。
5. **大输出工具的参数纪律**：日志要 tag/行数、包列表要分页、进程要过滤，避免把上下文撑爆。
6. **选择器稳定性排序**：`id` > `id_contains` > `desc` > `text` > `text_contains` > `class_contains` > 坐标。
7. **错误处理清单**：`could not get idle state`、元素被遮挡点了没反应、`verified=false`、
   隧道 `device offline` 等 20+ 场景的正确反应。
8. **安全边界**：`read_only` / `exec_allowlist` 闸门的表现与处置；破坏性命令必须先问用户。

---

## 安装

### 方式一：让 AI 自己装（推荐）

把仓库 `README.md` 里的 **[「一键安装提示词」](../README.md#十八一键安装提示词复制给-ai)** 整段复制给你的 AI 助手，
它会自行下载技能包、放到自己的 skills 目录，并引导你装手机端模块。

### 方式二：手动装

```bash
# 1) 取技能包（二选一）
#    a. 从 Release 下载
curl -LO https://github.com/HZY-520/KSU-MCP/releases/latest/download/ksu-mcp-skills-v1.3.0.zip
unzip -o ksu-mcp-skills-v1.3.0.zip -d /tmp/ksu-skills
#    b. 或直接用仓库
git clone --depth 1 https://github.com/HZY-520/KSU-MCP.git /tmp/ksu-mcp

# 2) 安装到你的客户端 skills 目录
mkdir -p ~/.claude/skills && cp -r /tmp/ksu-skills/ksu-mcp ~/.claude/skills/     # Claude Code
mkdir -p ~/.dsh/skills    && cp -r /tmp/ksu-skills/ksu-mcp ~/.dsh/skills/        # DeepSeek Harness
# 其他客户端：把它放进对应 skills/ 目录，并保持 SKILL.md 在 ksu-mcp/ 根、reference/ 与 examples/ 不变

# 3) 校验安装完整
ls ~/.claude/skills/ksu-mcp/SKILL.md ~/.claude/skills/ksu-mcp/reference/*.md
```

> **不支持目录式技能的客户端**（如某些网页版/自建 Agent）：直接把 `ksu-mcp/SKILL.md`
> 全文粘进系统提示，并把 `reference/` 三个文件放在可检索的位置即可，效果基本一致。

### 方式三：只装 MCP、不装技能

技能是可选的「说明书」。MCP 服务端本身自带完整工具描述，不装技能也能用，
只是 AI 需要自己摸索调用顺序，容易走「截图 + 估坐标」的费钱路线。

---

## 与 MCP 服务端的版本对应

| 技能包 | 服务端 | 说明 |
|---|---|---|
| v1.3.0 | ≥ v1.3.0 | 技能中的控件树工具（`android_get_screen_elements` 等 8 个）需要服务端 v1.3.0+；旧服务端上这些工具不存在，技能会引导 AI 退回截图方案 |

技能包版本号与服务端版本号同步发布，便于对应。
