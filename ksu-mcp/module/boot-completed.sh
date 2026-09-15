#!/system/bin/sh
# ------------------------------------------------------------------
# KSU MCP Server - 开机完成后的保活脚本（KernelSU 专用阶段）
# 兜底确保 watchdog 守护进程运行（watchdog 负责 daemon 与隧道的拉起保活）
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
[ "$(cat $DATA_DIR/disabled 2>/dev/null)" = "1" ] && exit 0

mkdir -p "$DATA_DIR"
[ -f "$CONFIG" ] || "$BIN" init-config >/dev/null 2>&1

# 统一守护入口：watchdog 每 10s 巡检 daemon 与隧道，异常自动拉起
if ! "$BIN" watchdog-status 2>/dev/null | grep -q '"running": true'; then
    "$BIN" watchdog --detach
fi

exit 0