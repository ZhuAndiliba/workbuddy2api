#!/usr/bin/env bash
# cn.sh — CN 实例（原版二进制）本机启停。
#
# 故意放在父目录而不是 upstream/ 里：upstream 是纯净的 git 克隆，日常 git pull
# 不应被本地脚本打扰（万一上游哪天也加了 run.sh，未跟踪文件会让 pull 拒绝合并）。
# pid/日志路径与中控约定一致：upstream/data/run/cn.pid、upstream/data/logs/cn.log，
# 因此这里启动的实例中控能识别、能停。
#
# 用法: ./cn.sh [start|stop|restart|status|logs]，缺省 status
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
DIR="$ROOT/upstream"
PID="$DIR/data/run/cn.pid"
LOG="$DIR/data/logs/cn.log"
PORT=7864

running() { [[ -f "$PID" ]] && kill -0 "$(cat "$PID" 2>/dev/null)" 2>/dev/null; }

start() {
    if running; then
        echo "[cn] 已在运行 (pid $(cat "$PID"))"
        return 0
    fi
    [[ -x "$DIR/wb2api" ]] || { echo "缺少 $DIR/wb2api（用 Docker 从 upstream 源码编译，见 README）" >&2; exit 1; }
    mkdir -p "$(dirname "$PID")" "$(dirname "$LOG")"
    echo "==> 启动 CN 实例 (原版二进制, :$PORT)"
    # 必须在 upstream/ 里起进程：config 的 auth_dir/state_file 都是相对路径，
    # 二进制按进程 cwd 解析。exec 让 $! 直接就是 wb2api 的 pid（与 run.sh 同款）。
    ( cd "$DIR" && exec nohup ./wb2api -config config.json >>"$LOG" 2>&1 ) &
    echo $! >"$PID"
    sleep 1
    if running; then
        echo "[cn] 已启动 (pid $(cat "$PID"))，日志: $LOG"
    else
        echo "[cn] 启动失败，日志尾部：" >&2
        tail -20 "$LOG" >&2
        exit 1
    fi
}

stop() {
    if ! running; then
        echo "[cn] 未在运行"
        rm -f "$PID"
        return 0
    fi
    local pid; pid="$(cat "$PID")"
    echo "==> 停止 [cn] (pid $pid)"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do running || break; sleep 0.1; done
    running && { echo "[cn] 5s 未退出，强制结束" >&2; kill -9 "$pid" 2>/dev/null || true; }
    rm -f "$PID"
    echo "[cn] 已停止"
}

case "${1:-status}" in
    start)   start ;;
    stop)    stop ;;
    restart) stop; start ;;
    status)
        if running; then
            printf "cn       运行中 (pid %s) :%s  " "$(cat "$PID")" "$PORT"
        else
            printf "cn       未运行            :%s  " "$PORT"
        fi
        echo "healthz=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$PORT/healthz" 2>/dev/null || echo '---')"
        ;;
    logs)    tail -f "$LOG" ;;
    *)       echo "用法: ./cn.sh [start|stop|restart|status|logs]" >&2; exit 1 ;;
esac
