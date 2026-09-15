#!/system/bin/sh
# ------------------------------------------------------------------
# KSU MCP Server - 开机自启脚本（service 阶段）
# KernelSU 与 Magisk 均会执行本脚本；幂等，可重复运行
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
LOG=$DATA_DIR/mcpd.log

export PATH=/system/bin:/system/xbin:/sbin:$PATH

[ -f "$BIN" ] || exit 0
[ -x "$BIN" ] || chmod 0755 "$BIN"

# 用户可通过创建 disabled 标记文件关闭自启：touch /data/adb/ksu_mcp/disabled
[ "$(cat $DATA_DIR/disabled 2>/dev/null)" = "1" ] && exit 0

mkdir -p "$DATA_DIR"
[ -f "$CONFIG" ] || "$BIN" init-config >/dev/null 2>&1

# 幂等启动：未运行时才拉起
if ! "$BIN" status 2>/dev/null | grep -q '"running": true'; then
    "$BIN" daemon --detach
fi

# 内网穿透：配置启用时自动拉起隧道客户端（设备侧；服务端见 tunnel-server/）
if "$BIN" tunnel-status 2>/dev/null | grep -q '"enabled": true'; then
    if ! "$BIN" tunnel-status 2>/dev/null | grep -q '"running": true'; then
        "$BIN" tunnel --detach
    fi
fi

exit 0
