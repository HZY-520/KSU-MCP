#!/system/bin/sh
# ------------------------------------------------------------------
# KSU MCP Server - 开机自启脚本（service 阶段）
# KernelSU 与 Magisk 均会执行本脚本；幂等，可重复运行
#
# v1.2.0：统一由 watchdog 进程守护接管。watchdog 每 10s 巡检，
# 自动拉起/重启 mcpd daemon 与内网穿透隧道；由本脚本（init 上下文）
# 拉起的 watchdog 不受 KSU Manager 页面/进程生命周期影响，
# 关闭 Manager 后服务照常后台运行。
# ------------------------------------------------------------------

# 定位模块目录（与 customize.sh 相同的兼容策略）
if [ -n "$MODPATH" ] && [ -f "$MODPATH/module.prop" ]; then
    MODDIR="$MODPATH"
elif [ -f "${0%/*}/module.prop" ]; then
    MODDIR="${0%/*}"
else
    MODDIR=$(pwd)
fi

BIN=$MODDIR/bin/mcpd
DATA_DIR=/data/adb/ksu_mcp
CONFIG=$DATA_DIR/config.json

export PATH=/system/bin:/system/xbin:/sbin:$PATH

[ -f "$BIN" ] || exit 0
[ -x "$BIN" ] || chmod 0755 "$BIN"

# 用户可通过创建 disabled 标记文件关闭自启：echo 1 > /data/adb/ksu_mcp/disabled
[ "$(cat $DATA_DIR/disabled 2>/dev/null)" = "1" ] && exit 0

mkdir -p "$DATA_DIR"
[ -f "$CONFIG" ] || "$BIN" init-config >/dev/null 2>&1

# watchdog 为统一守护入口：负责拉起并保活 mcpd daemon 与隧道
# watchdog-status 退出码：0=运行中，2=未运行（不依赖 JSON 文本解析）
if ! "$BIN" watchdog-status >/dev/null 2>&1; then
    "$BIN" watchdog --detach
fi

exit 0