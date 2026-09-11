# syntax=docker/dockerfile:1
# 多阶段构建：builder 里编出全部二进制，runtime 只带二进制 + 脚本。
# 注意：容器内跑的是"两个实例 + 控制台"（见 docker-entrypoint.sh），
# 端口 7864/7865 是 CN / 国际站，7860 是控制台。

FROM golang:1.23-alpine AS build
WORKDIR /src
# 先只拷依赖清单，让 go mod download 这层能被缓存
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 → 静态链接，运行镜像不需要 libc，也便于跨平台交叉编译
RUN set -eux; \
    for cmd in server console login signin credit; do \
        out=wb2api; [ "$cmd" = console ] && out=console; \
        [ "$cmd" = login ] && out=login; \
        [ "$cmd" = signin ] && out=signin_bin; \
        [ "$cmd" = credit ] && out=credit; \
        CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "/out/$out" "./cmd/$cmd"; \
    done

FROM alpine:3.20
# ca-certificates：出站 HTTPS 必需；tzdata：签到/保活的本地时区；curl：run.sh 的状态查询用
RUN apk add --no-cache ca-certificates tzdata curl \
 && mkdir -p /app/auths /app/data

WORKDIR /app
COPY --from=build /out/ /app/
COPY run.sh docker-entrypoint.sh /app/
RUN chmod +x /app/wb2api /app/console /app/login /app/signin_bin /app/credit \
              /app/run.sh /app/docker-entrypoint.sh

# 7864 CN · 7865 国际站 · 7860 控制台
EXPOSE 7860 7864 7865

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD curl -fsS http://127.0.0.1:7864/healthz >/dev/null 2>&1 \
   || curl -fsS http://127.0.0.1:7865/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/docker-entrypoint.sh"]
