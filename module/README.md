# KSU MCP Server · 模块包内说明（v1.2.0）

> 完整文档见仓库根目录 `README.md`。本文件只说明**模块包内**的内容与运维要点。

## 一、本模块安装后做了什么

1. **架构适配**：`customize.sh` 读取 `uname -m`，只保留匹配的二进制
   （`bin/arm64/mcpd` 或 `bin/arm/mcpd`），移动到 `bin/mcpd` 并 `chmod 0755`，
   删除另一架构目录，避免占用模块分区空间。
2. **数据目录**：`/data/adb/ksu_mcp`（安装/升级均保留）
   - `config.json` 运行配置（含鉴权 Token，权限 0600）
   - `mcpd.pid` / `tunnel.pid` / `watchdog.pid` 进程标识
   - `mcpd.runtime.json` 实际生效的 daemon 配置（供状态查询）
   - `tunnel.runtime.json` 隧道运行态（连接态 / 延迟 / 重连次数 / 最后错误）
   - `mcpd.log` / `tunnel.log` / `watchdog.log` 日志
   - `tmp/` 截屏临时文件（最多保留 5 张）
   - `disabled` 标记文件（存在即关闭开机自启）
3. **开机自启**：`service.sh`（service 阶段）与 `boot-completed.sh`（KernelSU 专用阶段）
   都在 init 上下文调用 `mcpd watchdog --detach`。
   两处都用 **`watchdog-status` 的退出码**判断（0=运行中 / 2=未运行），
   不再依赖 `grep '"running": true'` 这种一旦 JSON 格式微调就失效的写法。
4. **命令包装器**：`system/bin/mcpd` → `exec /data/adb/modules/ksu_mcp/bin/mcpd "$@"`，
   安装后可直接在终端执行 `mcpd status`。
5. **WebUI**：`webroot/index.html` 由 KernelSU Manager 内嵌 WebView 打开，
   通过注入的 `ksu` 桥接对象执行命令；也可通过设备端只读 API（`/api/state`）读取状态。

## 二、模块目录结构

```
ksu_mcp/
├── module.prop              # 模块元信息（version=v1.2.0 / versionCode=3）
├── customize.sh             # 安装脚本（架构检测 / 保留二进制 / 初始化配置 / 拉起守护）
├── service.sh               # 开机自启（service 阶段）
├── boot-completed.sh        # 开机完成兜底保活（boot-completed 阶段）
├── sepolicy.rule            # SELinux 规则补充（默认空，见文件内注释）
├── system/bin/mcpd          # PATH 包装器
├── webroot/index.html       # WebUI 控制台
└── bin/
    ├── arm64/mcpd           # 安装时二选一保留
    └── arm/mcpd
```

## 三、进程模型

```
service.sh / boot-completed.sh（init 上下文）
        └── mcpd watchdog（每 10s 巡检，setsid 独立会话，pid 写入 watchdog.pid）
              ├── mcpd daemon（Streamable HTTP + SSE，127.0.0.1:9123）
              │     └── /health 探活，连续 3 次失败 → 强杀重启
              └── mcpd tunnel（隧道客户端，配置启用时）
                    └── 进程不存在 → 拉起
                        运行态停滞 >45s 且已过计划重连时刻（连续 3 次）→ 判定卡死，强杀重启
```

`mcpd stop` 按 **tunnel → daemon → watchdog** 顺序停止（先断转发，最后停守护，避免被自动拉起）。

## 四、常用运维命令

```sh
mcpd status                                  # 运行状态（JSON）
mcpd start | restart | stop                  # 启动 / 重启 / 停止全部
mcpd watchdog-status                         # 守护状态（退出码 0=运行中 / 2=未运行）
mcpd tunnel-status                           # 隧道状态（真实连接态 + 质量指标）
mcpd tools                                   # 已注册的 MCP 工具列表
mcpd logs 200 --source watchdog              # 守护日志（查卡死强杀记录）
mcpd logs 200 --source tunnel                # 隧道日志（查重连原因）
curl -s http://127.0.0.1:9123/health         # 健康检查（免鉴权）
```

## 五、故障排查速查

| 现象 | 处理 |
|---|---|
| 服务不启动 | `mcpd logs --source watchdog`；确认 `/data/adb/ksu_mcp/disabled` 不存在 |
| 修改配置后未生效 | 配置需重启生效：WebUI「保存并重启服务」或 `mcpd restart` |
| 端口被占用 | 改端口：`mcpd config-set --port <新端口>` 后 `mcpd restart` |
| 局域网访问被拒 | `bind` 必须是 `0.0.0.0`，且 **必须**配置 Token（非回环地址无 Token 会拒绝启动） |
| 不想开机自启 | `touch /data/adb/ksu_mcp/disabled`（删除即恢复） |
| WebUI 显示只读降级 | 必须从 KernelSU Manager 打开（普通浏览器无 `ksu` 桥接对象） |
| 卸载 | Manager 中移除模块；如需彻底清理：`rm -rf /data/adb/ksu_mcp` |

## 六、升级与回滚

- **升级**：Manager 中直接安装新 zip 覆盖即可，`/data/adb/ksu_mcp` 自动保留；
  建议升级后进 WebUI 确认隧道已连接（v1.2.0 换了心跳与运行态机制）。
- **回滚**：重新安装旧版 zip 即可。注意旧版不认识 `tunnel.runtime.json`（会忽略，无副作用）。
