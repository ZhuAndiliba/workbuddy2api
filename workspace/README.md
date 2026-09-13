# wb2api 工作区

三个项目各司其职：**原版跑 CN（Docker 容器），国际版（我们的 fork）跑国际站，中控管两个。**

```
wb2api/
├── upstream/              原版克隆（github.com/Sliverkiss/workbuddy2api）
│     · CN 实例的【源码 + 配置 + 凭证】；.git 保留，git pull 永远干净
│     · 不再直接跑进程——由 docker-compose 构建成 wb2api-cn:local 容器（:7864）
├── international/         我们的 fork（github.com/ZhuAndiliba/workbuddy2api）
│     · 分支 feat/dual-region-web-console = 国际版
│     · 跑国际站 :7865（本机进程），config.json 只含 instances.global
│     · ./run.sh start|stop|restart|status|logs
├── console/               中控（网页 :7860）
│     · config.json 给每个实例写 root 指向其目录
│     · ./run.sh start|stop|status|logs
├── docker-compose.yml     CN 容器编排（restart: unless-stopped → 开机自动拉起）
├── cn.sh                  CN 容器启停 / 更新（见下）
└── growth.sh              查 CN 账号成长活动状态（领猫/任务/体力，只读）
```

## 日常操作

```bash
./cn.sh status                    # CN 容器状态 + healthz
./cn.sh start | stop | restart | logs
./cn.sh update                    # ★ CN 更新一条龙：git pull upstream + 重建镜像 + 重启容器
./growth.sh ALL                   # 查活动参与：领猫/任务/体力（只读 dry-run）
cd international && ./run.sh      # 国际站
cd console && ./run.sh start      # 中控 → http://127.0.0.1:7860/?token=…
```

CN 是容器（`restart: unless-stopped`）：**开机/Docker 守护进程启动时自动拉起，
进程崩溃自动重启**，无需人工干预。中控对国际站全功能（启停/日志/签到/解冻）；
对 CN 可看状态/账号/模型（容器内进程不可由中控直接启停，且原版本就无 /admin 接口）。

## 更新上游代码（两条独立动线）

**CN —— 一条命令（git pull 的 Docker 版）：**

```bash
./cn.sh update    # = git -C upstream pull + docker build + 容器替换重启，业务中断约 3 秒
```

镜像从 `upstream/` 拉下来的源码构建，所以"更新"就是先 git pull 再重建——
上游的新功能（猫猫旅行、活跃上报、限流修复……）随 pull 即得。

**国际版 —— 定期合并**（要上游新功能时才做，有冲突要解）：

```bash
cd international
git fetch upstream && git merge upstream/master   # 解冲突 → 测试 → 重编 → 重启
git push fork master:feat/dual-region-web-console
# 重编（cmd 目录换 console 即编中控）：
docker run --rm -v "$PWD":/src -v wb2api-gomod:/go/pkg/mod -v wb2api-gocache:/root/.cache/go-build \
  -w /src -e GOOS=darwin -e GOARCH=arm64 -e CGO_ENABLED=0 \
  golang:1.23-alpine go build -trimpath -ldflags="-s -w" -o /src/wb2api ./cmd/server
cp console ../console/console   # 中控二进制更新后拷过去
```

## 服务器部署（git 拉取 + Docker）

```bash
mkdir -p ~/code/wb2api && cd ~/code/wb2api
git clone https://github.com/Sliverkiss/workbuddy2api.git upstream
git clone -b feat/dual-region-web-console \
     https://github.com/ZhuAndiliba/workbuddy2api.git international
bash international/workspace/setup.sh     # 铺 cn.sh/compose/README，console 骨架就位

# 三份配置（参考 upstream/config.example.json / international/config.example.json /
# console/console.config.example.json；api_key 与 console.token 必填）

# 凭证从旧机 scp（不进 git）：
#   scp 旧机:~/code/wb2api/upstream/auths/*.json      upstream/auths/
#   scp 旧机:~/code/wb2api/international/auths/*.json international/auths/
# Linux 上修容器挂载目录权限（镜像以 uid 10001 运行；OrbStack/macOS 自动映射无需处理）：
#   chown -R 10001:10001 upstream/auths upstream/data

./cn.sh start                              # 首次会自动构建镜像（约 1-2 分钟）
cd international && ./run.sh && cd ..      # 国际站
cd console && ./run.sh start               # 中控
```

**国内服务器 Docker Hub 拉不动时**（x509 证书错误 = DNS 污染），用镜像源补基础镜像：

```bash
docker pull docker.m.daocloud.io/library/alpine:3.20
docker tag  docker.m.daocloud.io/library/alpine:3.20 alpine:3.20
# golang:1.23-alpine 同理；补齐后 ./cn.sh build 可正常构建
# （cn.sh 构建时会自动去掉 Dockerfile 的 # syntax= 行，避开 BuildKit 对 Hub 的依赖）
```

## 端口与密钥

| 服务 | 端口 | 密钥位置 |
|---|---|---|
| CN（容器） | 7864 | `upstream/config.json` 的 `api_key` |
| 国际站 | 7865 | `international/config.json` 的 `instances.global.api_key` |
| 中控 | 7860 | `console/config.json` 的 `console.token`（访问用 `?token=`） |

账号凭证按区域分家：CN 3 个在 `upstream/auths/`（挂载进容器，token 刷新写回宿主机文件），
国际站 3 个在 `international/auths/`。CN 登录新账号用 `upstream/login.sh`
（容器会自动发现新凭证文件并热加载，无需重启）。
