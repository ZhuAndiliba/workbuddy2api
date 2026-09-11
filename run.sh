#!/usr/bin/env bash
# run.sh — 本地直接运行 workbuddy2api（不依赖 Docker）
#
# 一区一实例：CN 与国际站各跑一个进程、各占一个端口、各写一份 state 文件。
# 区域由 config 的 "region" 字段决定，账号按凭证 domain 自动归入对应实例。
#
# 用法:
#   ./run.sh                # 启动两个实例（后台，等价于 ./run.sh both -d）
#   ./run.sh cn             # 前台运行 CN 实例（:7864）
#   ./run.sh global         # 前台运行国际站实例（:7865）
#   ./run.sh cn -d          # 后台运行（写 data/run/<target>.pid 与 data/logs/<target>.log）
#   ./run.sh both -d        # 两个实例都后台运行（both 恒为后台，见下）
#   ./run.sh cn restart     # 重启
#   ./run.sh stop both      # 停止
#   ./run.sh status         # 查看两个实例状态与端口健康
#   ./run.sh logs cn        # 跟踪日志（tail -f）
#   ./run.sh build          # 仅编译（server 二进制）
#   ./run.sh console -d     # 启动控制台 :7860（网页查看状态 + 点击启停两实例）
#   ./run.sh console        # 控制台前台运行；console stop|restart|status|logs 同理
#   ./run.sh login global   # OAuth 登录国际站账号（透传给 login.sh）
#
# 注：both 只能后台运行——前台模式下 start 用 exec 替换进程，第二个实例会永远起不来，
# 故 both 自动强制 -d（并打印提示）。
#
# 端口/配置对应关系（可在 config.<target>.json 里改）:
#   cn     → :7864   config.cn.json     data/state.cn.json
#   global → :7865   config.global.json data/state.global.json
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

# configFor / labelFor / portFor：单一映射来源，避免散落硬编码。
configFor() {
    case "$1" in
        cn)     echo "config.cn.json" ;;
        global) echo "config.global.json" ;;
        *) echo "未知实例: $1（可选 cn | global）" >&2; return 1 ;;
    esac
}

labelFor() {
    case "$1" in
        cn)     echo "CN" ;;
        global) echo "国际站" ;;
        *) return 1 ;;
    esac
}

# portFor 从该实例的 config 里读 listen（去掉冒号），保证与配置始终一致。
portFor() {
    local cfg; cfg="$(configFor "$1")"
    python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['listen'].lstrip(':'))" "$cfg" 2>/dev/null
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

start() {
    local target="$1" daemon="$2"
    local cfg; cfg="$(configFor "$target")"
    [[ -f "$cfg" ]] || { echo "缺少配置文件 $cfg" >&2; exit 1; }

    if isRunning "$target"; then
        echo "[$target] 已在运行 (pid $(cat "$(pidFile "$target")"))，跳过启动"
        return 0
    fi

    mkdir -p "$RUN_DIR" "$LOG_DIR"
    local port; port="$(portFor "$target")"
    if [[ -n "$port" ]] && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "[$target] 端口 $port 已被占用，先停掉占用者再启动" >&2
        exit 1
    fi

    build

    echo "==> 启动 $(labelFor "$target") 实例 (:$port, config=$cfg)"
    if [[ "$daemon" == "-d" ]]; then
        nohup "$BIN" -config "$cfg" >>"$(logFile "$target")" 2>&1 &
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
        exec "$BIN" -config "$cfg"
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
    if isRunning "$target"; then
        printf "%-8s 运行中 (pid %s) :%s  " "$target" "$(cat "$(pidFile "$target")")" "$port"
    else
        printf "%-8s 未运行            :%s  " "$target" "$port"
    fi
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || echo "---")"
    echo "healthz=$code"
}

status() {
    echo "实例      状态                  端口   健康检查"
    echo "------------------------------------------------------"
    statusOne cn
    statusOne global
    echo
    echo "提示：healthz 503 = 无可用账号（该实例区域内没有 healthy 账号），并非进程挂了。"
}

# 展开 target：both → cn global
expandTargets() {
    if [[ "$1" == "both" || "$1" == "all" ]]; then
        echo "cn global"
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
    CONSOLE_PORT=7860

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
        if lsof -nP -iTCP:"$CONSOLE_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
            echo "[console] 端口 $CONSOLE_PORT 已被占用" >&2; exit 1
        fi
        console_build
        echo "==> 启动控制台 (:$CONSOLE_PORT)"
        if [[ "$c_daemon" == "-d" ]]; then
            nohup "$CONSOLE_BIN" -root "$(pwd)" >>"$CONSOLE_LOG" 2>&1 &
            echo $! >"$CONSOLE_PID"
            sleep 1
            if console_running; then
                echo "[console] 已后台启动 (pid $(cat "$CONSOLE_PID"))"
                echo "           打开 http://127.0.0.1:$CONSOLE_PORT/ 查看/启停两个实例"
            else
                echo "[console] 启动失败，日志尾部：" >&2; tail -20 "$CONSOLE_LOG" >&2; exit 1
            fi
        else
            echo "(前台运行，Ctrl-C 退出)"
            exec "$CONSOLE_BIN" -root "$(pwd)"
        fi
    fi
    exit 0
fi

# 参数解析：子命令（start/stop/restart/status/logs）与目标（cn/global/both/all）
# 顺序任意、都可缺省——"stop both" 与 "both stop" 等价，"stop" 缺省对 both 生效。
target=""
sub=""
daemon=""
for arg in "$@"; do
    case "$arg" in
        -d|--daemon) daemon="-d" ;;
        start|stop|restart|status|logs) sub="$arg" ;;
        cn|global|both|all) target="$arg" ;;
        *) echo "未知参数: $arg" >&2; usage ;;
    esac
done
target="${target:-both}"
sub="${sub:-start}"

# status 且目标为两实例：用带表头的汇总视图（单实例仍走逐行输出）。
if [[ "$sub" == "status" && "$(expandTargets "$target")" == *" "* ]]; then
    status
    exit 0
fi

# both 恒后台：前台模式下 start 走 exec 替换进程，第二个实例永远起不来。
# start 与 restart 都适用（restart 内部也要 start 一次）。
forced_daemon=""
if [[ "$(expandTargets "$target")" == *" "* && ( "$sub" == "start" || "$sub" == "restart" ) && "$daemon" != "-d" ]]; then
    daemon="-d"
    forced_daemon="both 需后台运行（前台会阻断第二个实例），已自动加 -d"
fi
[[ -n "$forced_daemon" ]] && echo "提示: $forced_daemon" >&2

for t in $(expandTargets "$target"); do
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
