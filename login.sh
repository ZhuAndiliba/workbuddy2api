#!/usr/bin/env bash
# login.sh — WorkBuddy OAuth 登录 → 落盘 auth 文件（支持 CN / 国际站）
#
# 用法:
#   ./login.sh              # 默认 CN（copilot.tencent.com）
#   ./login.sh global       # 国际站（www.workbuddy.ai）
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录
#   3. 回到这里按 y → poll 拿 token+uid+nickname → 签到 → 落盘 auths/workbuddy-<uid>.json
#   4. 重启 workbuddy2api 容器加载新账号
#
# 区域决定上游 host 与 Origin，两区凭证互不通用；落盘的 domain 字段决定
# 网关侧 region 判定（workbuddy.ai → 国际站），无需改配置。
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"

REGION="${1:-cn}"
case "$REGION" in
    cn)     ORIGIN="https://www.codebuddy.cn";   LABEL="CN" ;;
    global) ORIGIN="https://www.workbuddy.ai";   LABEL="国际站" ;;
    *) echo "未知区域: ${REGION}（可选 cn | global）" >&2; exit 1 ;;
esac

mkdir -p "$AUTH_DIR"

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录（${LABEL}）"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url "$REGION")

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll "$REGION") || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── 签到（按区域打对应 host，幂等不阻塞）───
python3 - "$ORIGIN" "$TOKEN" "$USER_ID" "$ENT_ID" "$DOMAIN" <<'PYEOF'
import json, sys, urllib.request, urllib.error

origin, token, uid, ent_id, domain = sys.argv[1:6]

req = urllib.request.Request(
    origin + "/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer " + token,
        "Accept": "application/json",
        "Content-Type": "application/json",
        "Origin": origin,
        "Referer": origin + "/",
        "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
        "X-User-Id": uid,
        **({"X-Enterprise-Id": ent_id, "X-Tenant-Id": ent_id} if ent_id else {}),
        **({"X-Domain": domain} if domain else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=${USER_ID}），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=${USER_ID}），新增 auth 文件"
    ACTION="新增"
fi
python3 - "$AUTH_FILE" "$ACTION" "$USER_ID" "$ENT_ID" "$NICKNAME" "$TOKEN" "$REFRESH" "$EXPIRES_AT" "$DOMAIN" <<'PYEOF'
import json, sys

path, action, uid, ent_id, nickname, token, refresh, expires_at, domain = sys.argv[1:10]

auth = {
    "account": {
        "uid": uid,
        "enterpriseId": ent_id,
        "nickname": nickname
    },
    "auth": {
        "accessToken": token,
        "refreshToken": refresh,
        "expiresAt": int(expires_at),
        "domain": domain
    }
}
with open(path, "w") as f:
    json.dump(auth, f, indent=1)
print(f"已保存（{action}）: {path}")
PYEOF
chmod 600 "$AUTH_FILE" 2>/dev/null || true

# ─── 让对应区域的本机实例重新加载账号 ──────────────────────
# 只重启该区域的实例（login.sh global 不会打扰 CN 实例）。
echo ""
CFG="config.${REGION}.json"
if [[ ! -f "$CFG" ]]; then
    echo "配置 $CFG 不存在，auth 文件已保存，启动时会自动加载"
elif ! command -v jq >/dev/null 2>&1 && ! command -v python3 >/dev/null 2>&1; then
    echo "缺 python3，无法读取端口；auth 文件已保存，实例重启后会自动加载"
else
    PORT=$(python3 -c "import json;print(json.load(open('$CFG')).get('listen',':7863').lstrip(':'))" 2>/dev/null || echo "")
    API_KEY=$(python3 -c "import json;print(json.load(open('$CFG')).get('api_key',''))" 2>/dev/null || echo "")
    if [[ -n "$PORT" ]] && curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then
        echo "重启 ${LABEL} 实例（:${PORT}）以加载新账号..."
        ./run.sh "$REGION" restart -d >/dev/null 2>&1 || echo "  ! 重启失败，可手动执行 ./run.sh $REGION restart -d"
        sleep 2
        if [[ -n "$API_KEY" ]]; then
            COUNT=$(curl -s "http://127.0.0.1:$PORT/status" -H "Authorization: Bearer $API_KEY" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
        else
            COUNT=$(curl -s "http://127.0.0.1:$PORT/status" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
        fi
        echo "服务已重启，${LABEL} 实例当前账号数: $COUNT"
    else
        echo "${LABEL} 实例未运行（:${PORT:-?}），auth 文件已保存，下次启动自动加载"
    fi
fi

echo ""
echo "============================================================"
echo "  登录完成！（${LABEL}）"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
