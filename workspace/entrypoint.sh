#!/bin/sh
# entrypoint.sh — 一体化容器入口：拉代码 → 构建 → 起三个服务 → 守护。
#
# 设计（部署诉求）：
#   1. 每次容器启动都 git pull 两份仓库——CN 仓库我们从不改，pull 永远是快进（无损）；
#      国际版是 fork，pull 拿到的是你自己合并后推上去的结果。
#   2. 用容器内的 Go 编译（宿主机不再需要工具链；产物落在挂载目录里）。
#   3. 起三个进程：CN（原版二进制）、国际站（我们的二进制）、中控。
#   4. 转守护循环：进程崩了自动拉起；中控写的 data/run/<name>.want=down 表示
#      "用户主动停了"，此时不再拉起（否则中控与守护进程会互相打架）。
#
# 环境变量：
#   WB2A_PULL=0        跳过 git pull（离线调试）
#   WB2A_SKIP_BUILD=1  跳过编译（只想起服务）
#   WB2A_GIT_TOKEN     私有仓库拉取令牌（可选）
set -eu

ROOT=/work
UPSTREAM="$ROOT/upstream"
INTL="$ROOT/international"
CONSOLE="$ROOT/console"

log() { echo "[entrypoint] $*"; }

# ── 1. 拉取两份仓库 ─────────────────────────────────────────
# https + 可选 token（容器里配 SSH key 太麻烦）。有 .git 就 pull，没有就 clone
# （首次部署挂载空目录时走 clone）。
with_token() {
    if [ -n "${WB2A_GIT_TOKEN:-}" ]; then
        echo "$1" | sed "s#https://#https://x-access-token:${WB2A_GIT_TOKEN}@#"
    else
        echo "$1"
    fi
}

pull_or_clone() {
    dir="$1" url="$2" branch="${3:-}"
    if [ -d "$dir/.git" ]; then
        log "拉取 $(basename "$dir")"
        # 直接按 URL 取（不经过 origin）：宿主机克隆可能配的是 SSH remote，
        # 而容器里没有 ssh。取回结果落在 FETCH_HEAD，再快进合并到当前分支。
        if [ -n "$branch" ]; then
            git -C "$dir" fetch "$(with_token "$url")" "$branch" || return 0
        else
            git -C "$dir" fetch "$(with_token "$url")" || return 0
        fi
        # 只快进：本地若有分叉提交（国际版自己合并过上游）就明确报出来，
        # 沿用当前版本继续跑，不擅自 merge/rebase。
        if ! git -C "$dir" merge --ff-only FETCH_HEAD; then
            log "警告：$(basename "$dir") 不能快进（本地有分叉提交？），沿用当前版本"
        fi
    else
        log "克隆 $(basename "$dir")"
        if [ -n "$branch" ]; then
            git clone --branch "$branch" "$(with_token "$url")" "$dir"
        else
            git clone "$(with_token "$url")" "$dir"
        fi
    fi
}

if [ "${WB2A_PULL:-1}" = "1" ]; then
    for d in "$UPSTREAM" "$INTL"; do
        git config --global --add safe.directory "$d" 2>/dev/null || true
    done
    pull_or_clone "$UPSTREAM" "https://github.com/Sliverkiss/workbuddy2api.git"
    pull_or_clone "$INTL" "https://github.com/ZhuAndiliba/workbuddy2api.git" "feat/dual-region-web-console"
else
    log "WB2A_PULL=0，跳过拉取"
fi

# ── 2. 编译（容器内 Go；宿主机无需工具链）──────────────────
build_one() {
    dir="$1" out="$2" pkg="$3"
    if [ "${WB2A_SKIP_BUILD:-0}" = "1" ] && [ -x "$dir/$out" ]; then
        log "跳过编译 $out（已存在）"
        return 0
    fi
    log "编译 $out ← $pkg"
    ( cd "$dir" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$out" "$pkg" )
}

build_one "$UPSTREAM" wb2api ./cmd/server
build_one "$INTL" wb2api ./cmd/server
# 中控直接编到它自己的工作目录：容器里必须用 Linux 二进制，
# 而 $CONSOLE 是宿主机挂进来的目录（里面可能有旧的 macOS 二进制）。
log "编译 console → $CONSOLE/console"
( cd "$INTL" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$CONSOLE/console" ./cmd/console )

# ── 3. 启动服务 ─────────────────────────────────────────────
for d in "$UPSTREAM" "$INTL" "$CONSOLE"; do
    mkdir -p "$d/data/run" "$d/data/logs"
done

# 缺配置时从 example 生成并警告（不能让容器空跑成"看起来健康"）。
if [ ! -f "$UPSTREAM/config.json" ] && [ -f "$UPSTREAM/config.example.json" ]; then
    cp "$UPSTREAM/config.example.json" "$UPSTREAM/config.json"
    log "警告：upstream/config.json 缺失，已用 example 生成——请改 api_key 后重启容器"
fi
if [ ! -f "$INTL/config.json" ] && [ -f "$INTL/config.example.json" ]; then
    cp "$INTL/config.example.json" "$INTL/config.json"
    log "警告：international/config.json 缺失，已用 example 生成——请改 api_key 后重启容器"
fi
if [ ! -f "$CONSOLE/config.json" ]; then
    log "错误：缺少 $CONSOLE/config.json（中控：listen/token/instances）"
    exit 1
fi

# 日志双通道：服务自己写文件（中控日志卡片读它），tail -F 再镜像到容器 stdout
# （docker logs 可看）。tail 用 -F 以跟随日志轮转/重建。
#
# 注意：$! 必须是**服务本身**的 pid，守护循环靠它判活，所以用 exec 让子 shell
# 直接被目标进程替换；镜像用的 tail 单独起，不占这个 pid。
start_tail() {
    name="$1" dir="$2"
    ( nohup tail -F "$dir/data/logs/$name.log" 2>/dev/null | sed "s/^/[$name] /" ) &
}

start_svc() {
    name="$1" dir="$2"
    shift 2
    log "启动 $name"
    ( cd "$dir" && exec nohup "$@" >> "$dir/data/logs/$name.log" 2>&1 ) &
    echo $! > "$dir/data/run/$name.pid"
}

# 日志镜像只随容器起一次：它跟随文件，服务重启后仍继续输出，
# 放进 start_svc 会导致每次重启都多一个 tail 进程（实测会堆积上百个）。
for pair in "cn:$UPSTREAM" "global:$INTL" "console:$CONSOLE"; do
    start_tail "${pair%%:*}" "${pair#*:}"
done

# CN：原版二进制，单实例配置（不认识 -instance）
start_svc cn "$UPSTREAM" ./wb2api -config config.json
# 国际站：我们的二进制，配置含 instances.global
start_svc global "$INTL" ./wb2api -config config.json -instance global
# 中控：-supervisor 让它把启停写成意图文件，交给本脚本执行
start_svc console "$CONSOLE" ./console -root "$CONSOLE" -config config.json -supervisor

# 启动前复位意图文件：容器启动 = 全都起来（默认 up）。
# 不清掉的话，上次用中控停过的实例会在容器重启后继续保持停止，容易让人以为"坏了"。
for d in "$UPSTREAM" "$INTL" "$CONSOLE"; do
    for w in "$d"/data/run/*.want; do
        [ -f "$w" ] || continue
        if [ "$(tr -d '[:space:]' < "$w")" = "down" ]; then
            log "复位 $(basename "$w")=down → 容器启动默认全部拉起"
        fi
        rm -f "$w"
    done
done

# ── 4. 守护循环 ─────────────────────────────────────────────
# 每 5s 一轮：want=down → 停掉且不再拉起；want≠down 且进程不在 → 拉起。
svc_dir() {
    case "$1" in
        cn)     echo "$UPSTREAM" ;;
        global) echo "$INTL" ;;
        console) echo "$CONSOLE" ;;
    esac
}
svc_start() {
    case "$1" in
        cn)      start_svc cn "$UPSTREAM" ./wb2api -config config.json ;;
        global)  start_svc global "$INTL" ./wb2api -config config.json -instance global ;;
        console) start_svc console "$CONSOLE" ./console -root "$CONSOLE" -config config.json -supervisor ;;
    esac
}

supervise() {
    for name in cn global console; do
        dir="$(svc_dir "$name")"
        want_file="$dir/data/run/$name.want"
        pid_file="$dir/data/run/$name.pid"

        want=up
        [ -f "$want_file" ] && want="$(tr -d '[:space:]' < "$want_file")"

        pid=""
        [ -f "$pid_file" ] && pid="$(cat "$pid_file" 2>/dev/null || true)"
        alive=no
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then alive=yes; fi

        if [ "$want" = "down" ]; then
            if [ "$alive" = "yes" ]; then
                log "监督：停止 $name（中控请求）"
                kill "$pid" 2>/dev/null || true
            fi
        elif [ "$alive" = "no" ]; then
            log "监督：拉起 $name"
            svc_start "$name"
        fi
    done
}

term() {
    log "收到终止信号，停止所有服务…"
    for name in cn global console; do
        dir="$(svc_dir "$name")"
        pf="$dir/data/run/$name.pid"
        if [ -f "$pf" ]; then
            kill "$(cat "$pf" 2>/dev/null)" 2>/dev/null || true
        fi
    done
    exit 0
}
trap term INT TERM

log "进入守护循环（5s 一轮；中控启停经 data/run/<name>.want 生效）"
while :; do
    supervise
    sleep 5
done
