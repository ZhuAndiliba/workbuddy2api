#!/usr/bin/env bash
# setup.sh — 服务器组装三项目工作区：把胶水文件从 international/workspace 铺到父目录。
# 前提：父目录下已有 upstream/ 与 international/ 两份克隆（见 workspace/README.md）。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"   # .../international/workspace
ROOT="$(dirname "$HERE")"               # .../（父目录）

cp "$HERE/cn.sh" "$HERE/growth.sh" "$HERE/README.md" "$ROOT/"
chmod +x "$ROOT/cn.sh" "$ROOT/growth.sh"

mkdir -p "$ROOT/console/data/run" "$ROOT/console/data/logs"
cp "$HERE/console-run.sh" "$ROOT/console/run.sh"
chmod +x "$ROOT/console/run.sh"
if [[ ! -f "$ROOT/console/config.json" ]]; then
    cp "$HERE/console.config.example.json" "$ROOT/console/config.json"
    echo "已生成 console/config.json（按注释改 root 为服务器实际路径、填 token）"
fi
echo "工作区就位：$ROOT"
echo "下一步：编辑 upstream/config.json、international/config.json、console/config.json（见 README）"
