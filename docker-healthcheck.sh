#!/bin/sh
# docker-healthcheck.sh — 容器健康检查：任一实例可达（拿到任意 HTTP 状态码）即视为健康。
#
# 为什么不看 200：/healthz 在"进程活着但没有可用账号"时返回 503，这是正常业务状态
# （账号都在冷却/熔断/未登录），不该让容器被标记为 unhealthy。所以这里只判断
# "HTTP 服务是否响应"——连不上才算不健康。
#
# 端口从 config 读（支持改端口/加实例）：多实例格式逐实例取生效 listen，
# 单实例格式取顶层 listen；都读不到时回落默认的 7864/7865。
set -u

CONFIG="${WB2A_CONFIG:-/app/config.json}"

# 打印各实例的监听地址（每行一个）。读配置失败就什么都不打印。
listens() {
    names="$(/app/wb2api -config "$CONFIG" -list-instances 2>/dev/null || true)"
    if [ -n "$names" ]; then
        for n in $names; do
            /app/wb2api -config "$CONFIG" -instance "$n" -print-listen 2>/dev/null || true
        done
    else
        /app/wb2api -config "$CONFIG" -print-listen 2>/dev/null || true
    fi
}

found=0
for l in $(listens); do
    p="${l##*:}"
    [ -n "$p" ] || continue
    found=1
    if curl -s -o /dev/null --max-time 3 "http://127.0.0.1:$p/healthz"; then
        exit 0
    fi
done

# 配置读不出来时别直接判死：按默认端口再试一次。
if [ "$found" = 0 ]; then
    for p in 7864 7865; do
        curl -s -o /dev/null --max-time 3 "http://127.0.0.1:$p/healthz" && exit 0
    done
fi
exit 1
