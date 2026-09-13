#!/usr/bin/env bash
# run.sh — 中控（console）本机启停。
#
# 中控管两个目录里的实例（见 config.json 的 instances 分节，各带 root），
# 自己是个独立常驻进程，不随实例生灭。
#
# 用法: ./run.sh [start|stop|restart|status|logs]，缺省 start；-d 后台（默认）
set -euo pipefail
cd "$(dirname "$0")"

BIN="./console"
PID="data/run/console.pid"
LOG="data/logs/console.log"
PORT="7860"

running() { [[ -f "$PID" ]] && kill -0 "$(cat "$PID" 2>/dev/null)" 2>/dev/null; }

start() {
    if running; then
        echo "[console] 已在运行 (pid $(cat "$PID"))，跳过"
        return 0
    fi
    mkdir -p "$(dirname "$PID")" "$(dirname "$LOG")"
    if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "[console] 端口 $PORT 已被占用" >&2
        exit 1
    fi
    echo "==> 启动中控 (:$PORT)"
    nohup "$BIN" -root "$(pwd)" -config config.json >>"$LOG" 2>&1 &
    echo $! >"$PID"
    sleep 1
    if running; then
        echo "[console] 已启动 (pid $(cat "$PID"))"
        echo "           打开 http://127.0.0.1:$PORT/?token=<config 里的 token>"
    else
        echo "[console] 启动失败，日志尾部：" >&2
        tail -20 "$LOG" >&2
        exit 1
    fi
}

stop() {
    if ! running; then
        echo "[console] 未在运行"
        rm -f "$PID"
        return 0
    fi
    local pid; pid="$(cat "$PID")"
    echo "==> 停止 [console] (pid $pid)"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 30); do running || break; sleep 0.1; done
    running && kill -9 "$pid" 2>/dev/null || true
    rm -f "$PID"
    echo "[console] 已停止"
}

case "${1:-start}" in
    start)   start ;;
    stop)    stop ;;
    restart) stop; start ;;
    status)
        if running; then
            printf "console  运行中 (pid %s) :%s  " "$(cat "$PID")" "$PORT"
        else
            printf "console  未运行            :%s  " "$PORT"
        fi
        echo "http=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$PORT/" 2>/dev/null || echo '---')"
        ;;
    logs)    tail -f "$LOG" ;;
    *)       echo "用法: ./run.sh [start|stop|restart|status|logs]" >&2; exit 1 ;;
esac
