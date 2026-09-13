#!/usr/bin/env bash
# setup.sh — 服务器组装一体化容器工作区：铺编排文件 + 建目录骨架。
#
# 前提：父目录下已有 upstream/ 与 international/ 两份克隆（见 README）。
# 凭据与 config.json 不进 git，需要自己准备（见 README 的服务器部署一节）。
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"   # .../international/workspace
ROOT="$(dirname "$HERE")"               # .../（父目录）

cp "$HERE/Dockerfile" "$HERE/entrypoint.sh" "$HERE/docker-compose.yml" \
   "$HERE/run.sh" "$HERE/README.md" "$ROOT/"
chmod +x "$ROOT/run.sh" "$ROOT/entrypoint.sh"

mkdir -p "$ROOT/console"
cat > "$ROOT/console/config.json" <<'JSON'
{
  "console": {
    "listen": "0.0.0.0:7860",
    "token": "CHANGE-ME-强随机串（远程访问必填）",
    "supervisor": true
  },
  "instances": {
    "cn":     { "label": "CN",     "root": "../upstream" },
    "global": { "label": "国际站", "root": "../international" }
  }
}
JSON
echo "工作区就位：$ROOT"
echo "下一步："
echo "  1. 改 console/config.json 的 token"
echo "  2. 准备 upstream/config.json（CN，参考 config.example.json）"
echo "  3. 准备 international/config.json（国际站，参考 config.example.json）"
echo "  4. 放账号凭证：upstream/auths/ 与 international/auths/"
echo "  5. ./run.sh up"
