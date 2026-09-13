#!/usr/bin/env bash
# run.sh — 本地直接运行 workbuddy2api（不依赖 Docker）
#
# 一区一实例：CN 与国际站各跑一个进程、各占一个端口、各写一份 state 文件。
# 区域由 config 的 "region" 字段决定，账号按凭证 domain 自动归入对应实例。
#
# 用法:
#   ./run.sh                # 启动全部实例（后台，等价于 ./run.sh both -d）
#   ./run.sh cn             # 前台运行 CN 实例（:7864）
#   ./run.sh global         # 前台运行国际站实例（:7865）
#   ./run.sh <实例名> -d     # 配置里自定义的实例同样可直接当 target
#   ./run.sh cn -d          # 后台运行（写 data/run/<target>.pid 与 data/logs/<target>.log）
#   ./run.sh both -d        # 全部实例都后台运行（多实例恒为后台，见下）
#   ./run.sh cn restart     # 重启
#   ./run.sh stop both      # 停止
#   ./run.sh status         # 查看实例状态与端口健康
#   ./run.sh logs cn        # 跟踪日志（tail -f）
#   ./run.sh build          # 仅编译（server 二进制）
#   ./run.sh console -d     # 启动控制台 :7860（网页查看状态 + 点击启停实例）
#   ./run.sh console        # 控制台前台运行；console stop|restart|status|logs 同理
#   ./run.sh login global   # OAuth 登录国际站账号（透传给 login.sh）
#
# 注：多实例只能后台运行——前台模式下 start 用 exec 替换进程，后面的实例永远起不来，
# 故多个实例自动强制 -d（并打印提示）。
#
# 端口/配置对应关系（都在 config.json 的 instances 分节里改）:
#   cn     → :7864   instances.cn.listen      data/state.cn.json
#   global → :7865   instances.global.listen  data/state.global.json
# 实例名不限于 cn/global：配置里加一个实例，`./run.sh <名字> -d` 就能起它。
#
# 编译：优先用本地 Go；没有 Go 则用已交叉编译好的 ./wb2api（Docker 产出）。
set -euo pipefail

cd "$(dirname "$0")"

BIN="./wb2api"
RUN_DIR="./data/run"
LOG_DIR="./data/logs"

usage() {
    # 只打印开头的连续注释块（第 2 行起），到第一个非注释行为止——避免把脚本正文也打出来。
    awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0"
    exit 1
}

# CONFIG 单一配置文件，内含全部实例（见 config.json 的 instances 分节）。
CONFIG="${WB2A_CONFIG:-config.json}"

# labelFor：实例展示名。
labelFor() {
    case "$1" in
        cn)     echo "CN" ;;
        global) echo "国际站" ;;
        *) echo "$1" ;;
    esac
}

# 配置一律交给二进制自己解析（-print-listen / -list-instances）：走的是 server 同一套
# 合并逻辑（共享段 + instances 覆盖 + env），脚本里不必再写一份 JSON 解析。
# 二进制还没编出来时这些函数返回空，调用方需容忍"读不到"。

# portFor 该实例生效的监听端口（读不到返回空串）。
portFor() {
    [[ -x "$BIN" ]] || return 0
    "$BIN" -config "$CONFIG" -instance "$1" -print-listen 2>/dev/null | sed 's/.*://'
}

# hasInstance 判断该实例名是否在 config 里（单实例格式恒定通过）。
hasInstance() {
    [[ -x "$BIN" ]] || return 0
    local names; names="$("$BIN" -config "$CONFIG" -list-instances 2>/dev/null)" || return 0
    [[ -z "$names" ]] && return 0 # 单实例格式：不带 instances 分节，不做限制
    grep -qx -- "$1" <<<"$names"
}

pidFile() { echo "$RUN_DIR/$1.pid"; }
logFile() { echo "$LOG_DIR/$1.log"; }

# isRunning 返回 0 表示该实例进程存活（按 pid 文件 + kill -0 双校验）。
isRunning() {
    local pf; pf="$(pidFile "$1")"
    [[ -f "$pf" ]] || return 1
    local pid; pid="$(cat "$pf" 2>/dev/null || true)"
    [[ -n "$pid" ]] || return 1
    kill -0 "$pid" 2>/dev/null
}

build() {
    if command -v go >/dev/null 2>&1; then
        echo "==> 用本地 Go 编译 $BIN"
        CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/server
    elif [[ -x "$BIN" ]]; then
        echo "==> 未装 Go，沿用已有 ${BIN}（如需更新源码改动：装 Go 后重跑 ./run.sh build）"
    else
        cat >&2 <<'EOF'
错误：既没有本地 Go，也没有预编译的 ./wb2api。

二选一：
  a) 安装 Go：      brew install go && ./run.sh build
  b) 用 Docker 交叉编译出本机二进制（只需一次）：
       docker run --rm -v "$PWD":/src -w /src \
         -e GOOS=darwin -e GOARCH=arm64 -e CGO_ENABLED=0 \
         golang:1.23-alpine go build -trimpath -ldflags="-s -w" -o /src/wb2api ./cmd/server
EOF
        exit 1
    fi
}

# port_busy 端口占用探测：优先 lsof；没有 lsof 的 Linux 退回 bash /dev/tcp。
port_busy() {
    if command -v lsof >/dev/null 2>&1; then
        lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
    else
        (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
    fi
}

start() {
    local target="$1" daemon="$2"
    [[ -f "$CONFIG" ]] || { echo "缺少配置文件 $CONFIG" >&2; exit 1; }

    if isRunning "$target"; then
        echo "[$target] 已在运行 (pid $(cat "$(pidFile "$target")"))，跳过启动"
        return 0
    fi

    mkdir -p "$RUN_DIR" "$LOG_DIR"
    # 先确保二进制可用：实例名与端口都靠它读配置（见文件头 portFor/hasInstance）。
    build

    if ! hasInstance "$target"; then
        echo "[$target] 不在 $CONFIG 的 instances 分节里，跳过" >&2
        return 1
    fi

    local port; port="$(portFor "$target")"
    if [[ -z "$port" ]]; then
        echo "[$target] 读不到监听端口（$CONFIG 里该实例缺 listen？）" >&2
        return 1
    fi
    if port_busy "$port"; then
        echo "[$target] 端口 $port 已被占用，先停掉占用者再启动" >&2
        exit 1
    fi

    echo "==> 启动 $(labelFor "$target") 实例 (:$port, instance=$target, config=$CONFIG)"
    if [[ "$daemon" == "-d" ]]; then
        nohup "$BIN" -config "$CONFIG" -instance "$target" >>"$(logFile "$target")" 2>&1 &
        echo $! >"$(pidFile "$target")"
        sleep 1
        if isRunning "$target"; then
            echo "[$target] 已后台启动 (pid $(cat "$(pidFile "$target")"))，日志: $(logFile "$target")"
        else
            echo "[$target] 启动失败，日志尾部：" >&2
            tail -20 "$(logFile "$target")" >&2
            exit 1
        fi
    else
        echo "(前台运行，Ctrl-C 退出)"
        exec "$BIN" -config "$CONFIG" -instance "$target"
    fi
}

stop() {
    local target="$1"
    if ! isRunning "$target"; then
        echo "[$target] 未在运行"
        rm -f "$(pidFile "$target")"
        return 0
    fi
    local pid; pid="$(cat "$(pidFile "$target")")"
    echo "==> 停止 [$target] (pid $pid)"
    kill "$pid" 2>/dev/null || true
    # 优雅停机：给 5s 落盘时间（进程收到 SIGTERM 会先 Flush 状态再退出）。
    for _ in $(seq 1 50); do
        isRunning "$target" || break
        sleep 0.1
    done
    if isRunning "$target"; then
        echo "[$target] 未在 5s 内退出，强制结束" >&2
        kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$(pidFile "$target")"
    echo "[$target] 已停止"
}

statusOne() {
    local target="$1"
    local port; port="$(portFor "$target")"
    local portShow=":${port:-?}"
    if isRunning "$target"; then
        printf "%-8s 运行中 (pid %s) %-7s " "$target" "$(cat "$(pidFile "$target")")" "$portShow"
    else
        printf "%-8s 未运行            %-7s " "$target" "$portShow"
    fi
    if [[ -z "$port" ]]; then
        echo "healthz=--- （读不到端口：先 ./run.sh build 编出 wb2api）"
        return
    fi
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || echo "---")"
    echo "healthz=$code"
}

status() {
    echo "实例      状态                  端口   健康检查"
    echo "------------------------------------------------------"
    # 实例清单来自配置，不写死 cn/global（读不到配置时退回这两个）。
    local names=""
    if [[ -x "$BIN" ]]; then
        names="$("$BIN" -config "$CONFIG" -list-instances 2>/dev/null || true)"
    fi
    for t in ${names:-cn global}; do
        statusOne "$t"
    done
    echo
    echo "提示：healthz 503 = 无可用账号（该实例区域内没有 healthy 账号），并非进程挂了。"
}

# 展开 target：both/all → 配置里的全部实例（读不到配置时回落到 cn global）
# 输出统一用空格分隔：调用方靠 "含空格" 判断"是否多实例"，换行分隔会漏判。
expandTargets() {
    if [[ "$1" == "both" || "$1" == "all" ]]; then
        local names=""
        if [[ -x "$BIN" ]]; then
            names="$("$BIN" -config "$CONFIG" -list-instances 2>/dev/null || true)"
        fi
        echo "${names:-cn global}" | tr '\n' ' '
        echo
    else
        echo "$1"
    fi
}

# 独立命令（不接受 target/sub 参数），先于通用解析处理。
case "${1:-}" in
    -h|--help|help) usage ;;
    build) build; exit 0 ;;
    login)
        shift
        [[ $# -ge 1 ]] || { echo "用法: ./run.sh login <cn|global>" >&2; exit 1; }
        exec ./login.sh "$1"
        ;;
esac

# 控制台：常驻进程，提供网页查看两实例状态 + 点击启停。
# 它必须独立于被管的实例（面板 /ui 由实例自己提供，实例一停页面就没了），
# 故单独维护自己的 pid/日志，不套用下面按 region 的实例逻辑。
if [[ "${1:-}" == "console" ]]; then
    shift || true
    CONSOLE_BIN="./console"
    CONSOLE_PID="$RUN_DIR/console.pid"
    CONSOLE_LOG="$LOG_DIR/console.log"

    # 控制台端口同样问二进制（含 config 的 console 段与默认回落）；没编出来就先按默认。
    console_port() {
        local l=""
        if [[ -x "$CONSOLE_BIN" ]]; then
            l="$("$CONSOLE_BIN" -root "$(pwd)" -config "$CONFIG" -print-listen 2>/dev/null || true)"
        fi
        echo "${l##*:}"
    }

    c_daemon=""; c_sub="start"
    for arg in "$@"; do
        case "$arg" in
            -d|--daemon) c_daemon="-d" ;;
            start|stop|restart|status|logs) c_sub="$arg" ;;
            *) echo "未知参数: $arg" >&2; exit 1 ;;
        esac
    done

    console_running() {
        [[ -f "$CONSOLE_PID" ]] || return 1
        local pid; pid="$(cat "$CONSOLE_PID" 2>/dev/null || true)"
        [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null
    }
    console_build() {
        if command -v go >/dev/null 2>&1; then
            echo "==> 用本地 Go 编译 $CONSOLE_BIN"
            CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$CONSOLE_BIN" ./cmd/console
        elif [[ -x "$CONSOLE_BIN" ]]; then
            echo "==> 未装 Go，沿用已有 ${CONSOLE_BIN}"
        else
            echo "缺少 $CONSOLE_BIN：装 Go 后 ./run.sh build，或用 README 里的 Docker 交叉编译命令（把 ./cmd/server 换成 ./cmd/console）" >&2
            exit 1
        fi
    }
    console_stop() {
        if ! console_running; then echo "[console] 未在运行"; rm -f "$CONSOLE_PID"; return 0; fi
        local pid; pid="$(cat "$CONSOLE_PID")"
        echo "==> 停止 [console] (pid $pid)"
        kill "$pid" 2>/dev/null || true
        for _ in $(seq 1 30); do console_running || break; sleep 0.1; done
        console_running && kill -9 "$pid" 2>/dev/null || true
        rm -f "$CONSOLE_PID"
        echo "[console] 已停止"
    }

    case "$c_sub" in
        stop) console_stop; exit 0 ;;
        status)
            CONSOLE_PORT="$(console_port)"
            code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$CONSOLE_PORT/" 2>/dev/null || echo '---')"
            if console_running; then
                printf "console  运行中 (pid %s) :%s  http=%s\n" "$(cat "$CONSOLE_PID")" "$CONSOLE_PORT" "$code"
            else
                printf "console  未运行            :%s  http=%s" "$CONSOLE_PORT" "$code"
                [[ -x "$CONSOLE_BIN" ]] || printf "  (未编译)"
                echo
            fi
            exit 0
            ;;
        logs) exec tail -f "$CONSOLE_LOG" ;;
        restart) console_stop ;;
    esac

    if console_running; then
        echo "[console] 已在运行 (pid $(cat "$CONSOLE_PID"))，跳过启动"
    else
        mkdir -p "$RUN_DIR" "$LOG_DIR"
        console_build
        CONSOLE_PORT="$(console_port)"
        if port_busy "$CONSOLE_PORT"; then
            echo "[console] 端口 $CONSOLE_PORT 已被占用" >&2; exit 1
        fi
        echo "==> 启动控制台 (:$CONSOLE_PORT)"
        if [[ "$c_daemon" == "-d" ]]; then
            nohup "$CONSOLE_BIN" -root "$(pwd)" -config "$CONFIG" >>"$CONSOLE_LOG" 2>&1 &
            echo $! >"$CONSOLE_PID"
            sleep 1
            if console_running; then
                echo "[console] 已后台启动 (pid $(cat "$CONSOLE_PID"))"
                echo "           打开 http://127.0.0.1:$CONSOLE_PORT/ 查看/启停实例（监听与 token 见 config 的 console 段）"
            else
                echo "[console] 启动失败，日志尾部：" >&2; tail -20 "$CONSOLE_LOG" >&2; exit 1
            fi
        else
            echo "(前台运行，Ctrl-C 退出)"
            exec "$CONSOLE_BIN" -root "$(pwd)" -config "$CONFIG"
        fi
    fi
    exit 0
fi

# 参数解析：子命令（start/stop/restart/status/logs）与目标（实例名，或 both/all）
# 顺序任意、都可缺省——"stop both" 与 "both stop" 等价，"stop" 缺省对全部实例生效。
target=""
sub=""
daemon=""
unknown=""
for arg in "$@"; do
    case "$arg" in
        -d|--daemon) daemon="-d" ;;
        start|stop|restart|status|logs) sub="$arg" ;;
        cn|global|both|all) target="$arg" ;;
        *) [[ -z "$unknown" ]] && unknown="$arg" ;;
    esac
done
# 不限于 cn/global：配置里自定义的实例名也能直接当 target（先编出 wbapi 才查得到名字）。
if [[ -n "$unknown" ]]; then
    if [[ -z "$target" && -x "$BIN" ]] && hasInstance "$unknown"; then
        target="$unknown"
    else
        echo "未知参数: $unknown" >&2
        usage
    fi
fi
target="${target:-both}"
sub="${sub:-start}"

TARGETS="$(expandTargets "$target")"

# status 且目标多于一个：用带表头的汇总视图（单实例仍走逐行输出）。
if [[ "$sub" == "status" && "$TARGETS" == *" "* ]]; then
    status
    exit 0
fi

# 多实例恒后台：前台模式下 start 走 exec 替换进程，后面的实例永远起不来。
# start 与 restart 都适用（restart 内部也要 start 一次）。
forced_daemon=""
if [[ "$TARGETS" == *" "* && ( "$sub" == "start" || "$sub" == "restart" ) && "$daemon" != "-d" ]]; then
    daemon="-d"
    forced_daemon="多实例需后台运行（前台会阻断后续实例），已自动加 -d"
fi
[[ -n "$forced_daemon" ]] && echo "提示: $forced_daemon" >&2

for t in $TARGETS; do
    case "$sub" in
        start)   start "$t" "$daemon" ;;
        stop)    stop "$t" ;;
        restart) stop "$t"; start "$t" "$daemon" ;;
        status)  statusOne "$t" ;;
        logs)    tail -f "$(logFile "$t")" ;;
    esac
done

# 启动/重启后回显一次状态，省掉再敲一次 ./run.sh status。
if [[ "$sub" == "start" || "$sub" == "restart" ]] && [[ "$daemon" == "-d" ]]; then
    echo
    status
fi
