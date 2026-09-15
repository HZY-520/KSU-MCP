#!/system/bin/sh
# ------------------------------------------------------------------
# KSU MCP Server - KernelSU / Magisk 模块安装脚本
# 负责：架构检测、保留对应二进制、初始化数据目录与配置
# ------------------------------------------------------------------

# 定位模块目录：优先使用 KernelSU 通过环境变量提供的 MODPATH，
# 其次从 $0 推导，最后退回当前目录（兼容两种调用方式）
if [ -n "$MODPATH" ] && [ -f "$MODPATH/module.prop" ]; then
    MODDIR="$MODPATH"
elif [ -f "${0%/*}/module.prop" ]; then
    MODDIR="${0%/*}"
else
    MODDIR=$(pwd)
fi

# 兼容性兜底：部分环境可能未提供 ui_print / abort
if ! command -v ui_print >/dev/null 2>&1; then
    ui_print() { echo "$1"; }
fi
if ! command -v abort >/dev/null 2>&1; then
    abort() { echo "Abort: $1"; exit 1; }
fi

ui_print "*******************************"
ui_print "  KSU MCP Server v1.1.0"
ui_print "*******************************"

# 1. 检测 CPU 架构，保留对应二进制
ARCH=$(uname -m)
case "$ARCH" in
    aarch64|arm64)
        BIN_ARCH=arm64
        ;;
    armv7l|armv8l|arm*)
        BIN_ARCH=arm
        ;;
    *)
        abort "不支持的 CPU 架构: $ARCH（仅支持 arm64 / arm）"
        ;;
esac

ui_print "- 目标架构: $ARCH ($BIN_ARCH)"

[ -f "$MODDIR/bin/$BIN_ARCH/mcpd" ] || abort "缺少 $BIN_ARCH 架构的二进制文件（MODDIR=$MODDIR）"

mv -f "$MODDIR/bin/$BIN_ARCH/mcpd" "$MODDIR/bin/mcpd" || abort "无法移动二进制文件"
rm -rf "$MODDIR/bin/arm64" "$MODDIR/bin/arm"
chmod 0755 "$MODDIR/bin/mcpd"
chmod 0755 "$MODDIR/system/bin/mcpd"
ui_print "- 二进制就绪: bin/mcpd"

# 2. 初始化数据目录（跨模块升级保留）
DATA_DIR=/data/adb/ksu_mcp
mkdir -p "$DATA_DIR"

if [ ! -f "$DATA_DIR/config.json" ]; then
    "$MODDIR/bin/mcpd" init-config >/dev/null 2>&1 \
        && ui_print "- 已生成默认配置: $DATA_DIR/config.json" \
        || ui_print "- 警告: 配置初始化失败（稍后 service.sh 会重试）"
else
    ui_print "- 保留已有配置: $DATA_DIR/config.json"
fi

# 尝试立即拉起进程守护（部分环境安装后不会马上执行 service.sh，
# 这里直接启动 watchdog，由它负责 daemon 与隧道的拉起与保活）
"$MODDIR/bin/mcpd" watchdog --detach >/dev/null 2>&1 \
    && ui_print "- 进程守护已启动（每 10s 巡检 daemon 与隧道）" \
    || ui_print "- 进程守护将在下次开机/WebUI 操作时启动"

ui_print "- 安装完成，重启或打开 KernelSU 管理器中的 WebUI 生效"
exit 0
