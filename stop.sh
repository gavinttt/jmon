#!/usr/bin/env bash
#
# jmon 停止脚本
#
# 用法:
#   ./stop.sh           # 停止 daemon
#   ./stop.sh --force   # 忽略优雅退出，直接 kill -9
#
set -uo pipefail

cd "$(dirname "$0")"

BIN="./jmon"
PID_FILE="$HOME/.jmon/daemon.pid"
FORCE=0

for arg in "$@"; do
    case "$arg" in
        -f|--force) FORCE=1 ;;
        -h|--help)  sed -n '3,7p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "未知参数: $arg" >&2; exit 1 ;;
    esac
done

running_pid() {
    [[ -f "$PID_FILE" ]] || return 1
    local pid
    pid="$(tr -dc '0-9' < "$PID_FILE")"
    [[ -n "$pid" ]] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    echo "$pid"
}

if ! pid="$(running_pid)"; then
    echo "jmon 未在运行"
    rm -f "$PID_FILE"
    exit 0
fi

if [[ "$FORCE" -eq 1 ]]; then
    echo "==> 强制结束 PID $pid"
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$PID_FILE"
    echo "已停止"
    exit 0
fi

# 优先使用内置命令（按 PID 文件定位，优雅退出）
if [[ -x "$BIN" ]] && "$BIN" stop; then
    :
else
    echo "==> 内置 stop 失败，直接发送 SIGTERM 到 PID $pid"
    kill "$pid" 2>/dev/null || true
fi

# 等待进程真正退出，最多 5 秒
for _ in $(seq 1 25); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
done

if kill -0 "$pid" 2>/dev/null; then
    echo "警告: PID $pid 仍未退出，尝试 kill -9"
    kill -9 "$pid" 2>/dev/null || true
fi

rm -f "$PID_FILE"
echo "已停止"
