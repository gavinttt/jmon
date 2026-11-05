#!/usr/bin/env bash
#
# jmon 启动脚本
#
# 用法:
#   ./start.sh                      # 默认端口 9810，采集间隔 30s
#   ./start.sh -p 9820              # 指定端口
#   ./start.sh -i 60                # 指定采集间隔(秒)
#   ./start.sh -p 9820 -i 60        # 同时指定
#   ./start.sh --no-build           # 跳过编译，直接启动
#   ./start.sh --rebuild            # 强制重新编译后启动
#
set -euo pipefail

cd "$(dirname "$0")"

BIN="./jmon"
PORT=9810
INTERVAL=30
BUILD_MODE="auto"   # auto | skip | force

usage() {
    sed -n '3,12p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--port)     PORT="${2:-}"; shift 2 ;;
        -i|--interval) INTERVAL="${2:-}"; shift 2 ;;
        --no-build)    BUILD_MODE="skip"; shift ;;
        --rebuild)     BUILD_MODE="force"; shift ;;
        -h|--help)     usage ;;
        *) echo "未知参数: $1"; usage ;;
    esac
done

if ! [[ "$PORT" =~ ^[0-9]+$ ]] || ! [[ "$INTERVAL" =~ ^[0-9]+$ ]]; then
    echo "错误: 端口和采集间隔必须是数字" >&2
    exit 1
fi

# ---------- 编译 ----------
need_build() {
    [[ ! -x "$BIN" ]] && return 0
    # 任一 .go 文件比二进制新，则需要重新编译
    if [[ -n "$(find . -name '*.go' -not -path './.git/*' -newer "$BIN" -print -quit 2>/dev/null)" ]]; then
        return 0
    fi
    return 1
}

do_build() {
    if ! command -v go >/dev/null 2>&1; then
        echo "错误: 未找到 go 命令，且没有可用的 $BIN" >&2
        exit 1
    fi
    echo "==> 正在编译 jmon ..."
    go build -trimpath -ldflags "-s -w" -o "$BIN" .
    echo "==> 编译完成: $(ls -lh "$BIN" | awk '{print $5}')"
}

case "$BUILD_MODE" in
    force) do_build ;;
    skip)  [[ -x "$BIN" ]] || { echo "错误: $BIN 不存在，无法跳过编译" >&2; exit 1; } ;;
    auto)  if need_build; then do_build; else echo "==> $BIN 已是最新，跳过编译"; fi ;;
esac

# ---------- 端口占用检查 ----------
if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "警告: 端口 $PORT 已被占用（可能已有 jmon 在运行）" >&2
    echo "      可执行 ./stop.sh 后重试，或用 -p 指定其它端口" >&2
    exit 1
fi

# ---------- 启动 ----------
echo "==> 启动 jmon (port=$PORT, interval=${INTERVAL}s)"
"$BIN" --port "$PORT" --interval "$INTERVAL"

# 父进程只负责孵化，子进程脱离终端常驻
PID_FILE="$HOME/.jmon/daemon.pid"
for _ in 1 2 3 4 5 6 7 8 9 10; do
    [[ -f "$PID_FILE" ]] && break
    sleep 0.2
done

if [[ -f "$PID_FILE" ]]; then
    echo "==> 运行中 PID: $(cat "$PID_FILE")"
else
    echo "警告: 未读到 PID 文件，请检查日志" >&2
fi

echo "==> Dashboard : http://localhost:$PORT/dashboard"
echo "==> 日志       : $HOME/.jmon/daemon.log"
echo "==> 停止       : ./stop.sh"
