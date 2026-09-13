#!/usr/bin/env bash
# run.sh — 一体化容器（CN + 国际站 + 中控）控制入口。
#
# 一个容器装全部；每次启动都会 git pull 两份仓库再编译启动：
#   CN 仓库我们从不修改 → pull 永远快进（无损）；国际版拉你自己推的合并结果。
#
# 用法:
#   ./run.sh up        构建并启动（首次约 1-2 分钟）——开机自启由 restart 策略兜底
#   ./run.sh restart   重启 = 重新拉代码 + 编译 + 起服务（日常更新就用这个）
#   ./run.sh down      停止并删除容器（挂载的数据不受影响）
#   ./run.sh logs      跟踪三个服务的日志（带 [cn]/[global]/[console] 前缀）
#   ./run.sh status    容器状态 + 三个端口健康
#   ./run.sh build     只重建镜像（entrypoint/配置变了才需要）
#   ./run.sh shell     进容器
set -euo pipefail
cd "$(dirname "$0")"

case "${1:-status}" in
    up)      docker compose up -d --build ;;
    build)   docker compose build ;;
    restart) docker compose up -d --force-recreate ;;
    down)    docker compose down ;;
    logs)    docker compose logs -f ;;
    shell)   docker exec -it wb2api sh ;;
    status)
        docker compose ps
        echo
        for p in 7864 7865 7860; do
            printf ":%-5s → %s\n" "$p" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$p/healthz" 2>/dev/null || echo '---')"
        done
        ;;
    *) echo "用法: ./run.sh [up|restart|down|logs|status|build|shell]" >&2; exit 1 ;;
esac
