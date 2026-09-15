#!/system/bin/sh
# ------------------------------------------------------------------
# KSU MCP Server - 开机完成后的保活脚本（KernelSU 专用阶段）
# 若 service 阶段启动失败或进程退出，在这里兜底重启
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
LOG=$DATA_DIR/mcpd.log

export PATH=/system/bin:/system/xbin:/sbin:$PATH

[ -f "$BIN" ] || exit 0
[ "$(cat $DATA_DIR/disabled 2>/dev/null)" = "1" ] && exit 0

mkdir -p "$DATA_DIR"
[ -f "$DATA_DIR/config.json" ] || "$BIN" init-config >/dev/null 2>&1

if ! "$BIN" status 2>/dev/null | grep -q '"running": true'; then
    "$BIN" daemon --detach
fi

# 内网穿透保活
if "$BIN" tunnel-status 2>/dev/null | grep -q '"enabled": true'; then
    if ! "$BIN" tunnel-status 2>/dev/null | grep -q '"running": true'; then
        "$BIN" tunnel --detach
    fi
fi

exit 0
