#!/usr/bin/env bash
# growth.sh — 查询 CN 账号的成长活动状态（领猫 / 任务 / 体力 / 积分）。
#
# 用的是上游 scripts/task_runner.py（默认 dry-run 只读，不发任何写请求）。
# 上游脚本把凭证目录写死成了作者自己服务器的路径，这里每次运行时拷一份
# 修正路径再跑——不动 upstream/ 克隆，也永远用上游最新版脚本。
#
# 用法:
#   ./growth.sh ALL            # 查全部账号（只读）
#   ./growth.sh a40e07a6       # 查单个账号（uid 前缀）
#   ./growth.sh ALL --yes      # ⚠ 真实执行任务（会消耗体力/上报，写操作慎用）
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
CACHE="$ROOT/.growth-cache"
mkdir -p "$CACHE"

sed "s#AUTHS = \"/root/workbuddy2api/auths\"#AUTHS = \"$ROOT/upstream/auths\"#" \
    "$ROOT/upstream/scripts/task_common.py" > "$CACHE/task_common.py"
cp "$ROOT/upstream/scripts/task_runner.py" "$CACHE/"

if [[ "${1:-}" == "--help" || -z "${1:-}" ]]; then
    sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
fi
exec python3 "$CACHE/task_runner.py" "$@"
