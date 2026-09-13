#!/usr/bin/env bash
# cn.sh — CN 实例（Docker 容器，镜像从 upstream/ 的 git 源码构建）启停。
#
# 更新上游代码的完整动作：./cn.sh update  =  git pull + 重建镜像 + 重启容器。
# 开机/重启 Docker 守护进程时容器由 restart: unless-stopped 自动拉起。
#
# 日志旁路：容器日志在 docker logs（stdout），而中控网页读的是文件——
# 所以 start 时顺带起一个 `docker logs -f` 的旁路进程把日志落到
# upstream/data/logs/cn.log，中控的 CN 日志卡片因此照常工作。
#
# 用法: ./cn.sh [start|stop|restart|status|logs|update|build]，缺省 status
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

PID="$ROOT/upstream/data/run/cn-log.piper"   # 日志旁路进程的 pid
LOG="$ROOT/upstream/data/logs/cn.log"
PORT=7864

piper_running() { [[ -f "$PID" ]] && kill -0 "$(cat "$PID" 2>/dev/null)" 2>/dev/null; }

start_piper() {
    if ! piper_running; then
        mkdir -p "$(dirname "$PID")" "$(dirname "$LOG")"
        nohup docker logs -f --tail 0 wb2api-cn >>"$LOG" 2>&1 &
        echo $! >"$PID"
    fi
}

stop_piper() {
    if piper_running; then
        kill "$(cat "$PID")" 2>/dev/null || true
    fi
    rm -f "$PID"
}

# build_image 从 upstream/ 源码构建 wb2api-cn:local。
# 通过 stdin 传 Dockerfile 并去掉首行 # syntax= 指令：BuildKit 会为它去 Docker Hub
# 拉"构建前端"，在 Docker Hub 被污染/断网的机器上会直接失败；builtin 前端完全够用。
# 基础镜像（golang:1.23-alpine / alpine:3.20）本地缺失时用国内镜像源补：
#   docker pull docker.m.daocloud.io/library/alpine:3.20 && docker tag ... alpine:3.20
build_image() {
    sed '/^# syntax=/d' "$ROOT/upstream/Dockerfile" \
      | docker build -t wb2api-cn:local -f - "$ROOT/upstream"
}

case "${1:-status}" in
    start)
        docker image inspect wb2api-cn:local >/dev/null 2>&1 || build_image
        docker compose up -d cn
        sleep 1
        start_piper
        echo "[cn] 容器已启动；日志文件: ${LOG#$ROOT/}（docker logs 同步可见）"
        ;;
    stop)
        docker compose stop cn
        stop_piper
        echo "[cn] 容器已停止"
        ;;
    restart)
        docker compose stop cn
        stop_piper
        docker compose up -d cn
        sleep 1
        start_piper
        echo "[cn] 容器已重启"
        ;;
    update)
        echo "==> git pull upstream"
        git -C "$ROOT/upstream" pull
        echo "==> 重建镜像并重启容器"
        build_image
        docker compose up -d cn
        sleep 1
        start_piper
        docker compose ps cn
        ;;
    build)
        build_image
        ;;
    status)
        docker compose ps cn
        printf "healthz=%s\n" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$PORT/healthz" 2>/dev/null || echo '---')"
        ;;
    logs)
        tail -f "$LOG"
        ;;
    *)
        echo "用法: ./cn.sh [start|stop|restart|status|logs|update|build]" >&2
        exit 1
        ;;
esac
