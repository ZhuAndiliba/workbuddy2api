#!/bin/sh
# docker-entrypoint.sh — 容器内启动全部实例（config 的 instances 分节）+ 控制台。
#
# 设计要点：
#   - 实例清单来自 config 的 instances 分节（`wb2api -list-instances` 枚举），
#     加一个实例只改 config，不必动这个脚本；单实例格式（无 instances 分节）则不带
#     -instance 直接起一个。
#   - 子进程挂在容器 PID 1 下，日志直接进容器 stdout/stderr（docker logs 可看），
#     经 sed 加 [实例名] / [console] 前缀便于区分。
#   - 任一实例退出即视为容器异常（trap 收尾），避免"容器活着但服务已经死了"的假健康。
set -eu

cd /app

CONFIG="${WB2A_CONFIG:-config.json}"

if [ ! -f "$CONFIG" ]; then
    cat >&2 <<EOF
错误：/app/$CONFIG 不存在。

请从 config.example.json 复制一份并改好 api_key / listen / state_file：
  cp config.example.json config.json
用 docker-compose 时，该文件通过 volumes 挂进 /app。
EOF
    exit 1
fi

# 实例清单：交给二进制自己解析 JSON，容器内无需 python3/jq。
if ! INSTANCES="$(./wb2api -list-instances -config "$CONFIG" 2>&1)"; then
    echo "[entrypoint] 读 $CONFIG 的实例清单失败：" >&2
    echo "$INSTANCES" >&2
    exit 1
fi

pids=""

# run_instance 启动一个实例，日志加 [name] 前缀。$1 为实例名，空串表示单实例格式。
run_instance() {
    name="$1"
    if [ -n "$name" ]; then
        echo "[entrypoint] 启动实例 $name (config=$CONFIG)"
        ./wb2api -config "$CONFIG" -instance "$name" 2>&1 | sed "s/^/[$name] /" &
    else
        echo "[entrypoint] 启动单实例 (config=$CONFIG)"
        ./wb2api -config "$CONFIG" 2>&1 | sed "s/^/[wb2api] /" &
    fi
    pids="$pids $!"
}

if [ -n "$INSTANCES" ]; then
    for name in $INSTANCES; do
        run_instance "$name"
    done
    echo "[entrypoint] 共启动 $(echo "$INSTANCES" | wc -w) 个实例：$(echo "$INSTANCES" | tr '\n' ' ')"
else
    run_instance ""
fi

# 控制台：监听与 token 都读 config 的 console 段（见 config.example.json）。
# WB2A_CONSOLE_LISTEN / WB2A_CONSOLE_TOKEN 可临时覆盖（调试用）。
# console 自身有硬约束：非回环监听且无 token 会拒绝启动——所以容器里必须配 token。
set -- -root /app -config "$CONFIG"
if [ -n "${WB2A_CONSOLE_LISTEN:-}" ]; then set -- "$@" -listen "$WB2A_CONSOLE_LISTEN"; fi
if [ -n "${WB2A_CONSOLE_TOKEN:-}" ]; then set -- "$@" -token "$WB2A_CONSOLE_TOKEN"; fi
echo "[entrypoint] 启动控制台"
./console "$@" 2>&1 | sed 's/^/[console] /' &
pids="$pids $!"

# 收尾：任一子进程退出就停掉其余，避免容器假存活。
# shellcheck disable=SC2329  # 经 trap 调用
term() {
    echo "[entrypoint] 收到终止信号，停止所有子进程…"
    # shellcheck disable=SC2086
    kill $pids 2>/dev/null || true
    wait 2>/dev/null || true
    exit 0
}
trap term INT TERM

# shellcheck disable=SC3045  # wait -n 在 busybox ash（Alpine）上可用；失败则退回 wait
wait -n 2>/dev/null || wait
echo "[entrypoint] 有子进程退出，容器即将结束" >&2
# shellcheck disable=SC2086
kill $pids 2>/dev/null || true
exit 1
