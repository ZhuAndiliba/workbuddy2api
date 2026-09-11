#!/bin/sh
# docker-entrypoint.sh — 容器内启动 CN / 国际站两个实例 + 控制台。
#
# 设计要点：
#   - 只启实际存在配置的实例：想只跑 CN，就别挂 config.global.json（或删掉）；
#     两个都不在时会明确报错，而不是静默起一个空壳。
#   - 子进程挂在容器 PID 1 下，日志直接进容器 stdout/stderr（docker logs 可看）。
#   - 任一实例退出即视为容器异常（trap 收尾），避免"容器活着但服务已经死了"的假健康。
set -eu

cd /app

CONFIGS=""
for f in config.cn.json config.global.json; do
    [ -f "$f" ] && CONFIGS="$CONFIGS $f"
done

if [ -z "$CONFIGS" ]; then
    cat >&2 <<'EOF'
错误：/app 下没有 config.cn.json 也没有 config.global.json。

请从 config.example.json 复制一份并改好 api_key / listen / state_file，例如：
  cp config.example.json config.cn.json      # 再改 "region": "cn"
  cp config.example.json config.global.json  # 再改 "region": "global"
用 docker-compose 时，这两个文件通过 volumes 挂进 /app。
EOF
    exit 1
fi

pids=""

# run_instance 启动一个实例，日志加 [cn]/[global] 前缀，便于在 docker logs 里区分。
run_instance() {
    cfg="$1"
    name="$(echo "$cfg" | sed 's/^config\.//; s/\.json$//')"
    echo "[entrypoint] 启动实例 $name (config=$cfg)"
    ./wb2api -config "$cfg" 2>&1 | sed "s/^/[$name] /" &
    pids="$pids $!"
}

for cfg in $CONFIGS; do
    run_instance "$cfg"
done

# 控制台：默认绑回环。容器外要访问必须改成 0.0.0.0 并给 token
# （console 自身有硬约束：非回环监听且无 token 会拒绝启动）。
CONSOLE_LISTEN="${WB2A_CONSOLE_LISTEN:-0.0.0.0:7860}"
CONSOLE_TOKEN="${WB2A_CONSOLE_TOKEN:-}"
if [ -n "$CONSOLE_TOKEN" ]; then
    echo "[entrypoint] 启动控制台 (listen=$CONSOLE_LISTEN, token 已设置)"
    ./console -root /app -listen "$CONSOLE_LISTEN" -token "$CONSOLE_TOKEN" 2>&1 | sed 's/^/[console] /' &
else
    echo "[entrypoint] 启动控制台 (listen=$CONSOLE_LISTEN, 无 token)"
    echo "[entrypoint] 警告：容器内监听 0.0.0.0 且未设 WB2A_CONSOLE_TOKEN，控制台可被同网段访问；"
    echo "[entrypoint]       它可启停进程/读日志。建议设置 WB2A_CONSOLE_TOKEN 或把监听改回 127.0.0.1。"
    ./console -root /app -listen "$CONSOLE_LISTEN" 2>&1 | sed 's/^/[console] /' &
fi
pids="$pids $!"

# 收尾：任一子进程退出就停掉其余，避免容器假存活。
term() {
    echo "[entrypoint] 收到终止信号，停止所有子进程…"
    # shellcheck disable=SC2086
    kill $pids 2>/dev/null || true
    wait 2>/dev/null || true
    exit 0
}
trap term INT TERM

wait -n 2>/dev/null || wait
echo "[entrypoint] 有子进程退出，容器即将结束" >&2
# shellcheck disable=SC2086
kill $pids 2>/dev/null || true
exit 1
