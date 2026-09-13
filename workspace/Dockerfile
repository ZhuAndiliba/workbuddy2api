# wb2api 一体化容器：一个容器装全部（CN + 国际站 + 中控）。
#
# 与"每服务一个镜像"的区别：本镜像**不带业务代码**，只带工具链与入口脚本；
# 代码在容器启动时从 git 拉取（见 entrypoint.sh），因此"更新 = 重启容器"。
# 好处是 CN 仓库我们从不修改，pull 永远快进无损；国际版拉你自己推的合并结果。
#
# 构建：docker compose build   （首次约 1-2 分钟，之后有层缓存）
# 运行：docker compose up -d   （entrypoint 拉取 → 编译 → 起三个服务 → 守护）

FROM golang:1.23-alpine

# git：拉取两份仓库；ca-certificates：出站 HTTPS；tzdata：签到/旅行按本地时区触发；
# curl：健康检查与调试；bash：run.sh 系列脚本用（容器内我们自己的脚本是 sh，兼容）
RUN apk add --no-cache git ca-certificates tzdata curl bash \
 && mkdir -p /work/upstream /work/international /work/console

# Go 模块缓存放在卷里（compose 挂载），容器重建后不必重新下载依赖。
# 关键：-mod=readonly（Go 默认）——绝不能让编译改写挂载进来的 go.mod/go.sum，
# 那会弄脏上游仓库，破坏"pull 永远快进无损"的前提。
ENV GOMODCACHE=/go/pkg/mod \
    GOCACHE=/root/.cache/go-build \
    GOFLAGS=-mod=readonly

WORKDIR /work
COPY entrypoint.sh /work/entrypoint.sh
RUN chmod +x /work/entrypoint.sh

# 7864 CN · 7865 国际站 · 7860 中控
EXPOSE 7860 7864 7865

# 三个服务任一有响应即视为健康（503 = 活着但无可用账号，不算不健康）。
# 只探 CN/国际站：中控是运维面板，它挂了不影响 API。
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s \
  CMD curl -s -o /dev/null http://127.0.0.1:7864/healthz \
   || curl -s -o /dev/null http://127.0.0.1:7865/healthz || exit 1

ENTRYPOINT ["/work/entrypoint.sh"]
