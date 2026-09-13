# wb2api 工作区

三个项目各司其职：**原版跑 CN，国际版（我们的 fork）跑国际站，中控管两个。**

```
wb2api/
├── upstream/              原版克隆（github.com/Sliverkiss/workbuddy2api）
│     · .git 保留，git pull 永远干净（本地不加文件，二进制/配置/凭证均被 .gitignore）
│     · 跑 CN :7864，config.json / auths/ / data/ 都在这（不受版本管理）
├── international/         我们的 fork（github.com/ZhuAndiliba/workbuddy2api）
│     · 分支 feat/dual-region-web-console = 国际版
│     · 跑国际站 :7865，config.json 只含 instances.global
│     · ./run.sh start|stop|restart|status|logs（裸 ./run.sh = 起国际站）
├── console/               中控（网页 :7860，管上面两个目录里的实例）
│     · config.json 给每个实例写 root 指向其目录
│     · ./run.sh start|stop|status|logs
├── cn.sh                  CN 实例本机启停（放父目录，保持 upstream 克隆纯净）
└── growth.sh              查 CN 账号成长活动状态（领猫/任务/体力，只读）
```

## 日常操作

```bash
./cn.sh status                    # CN（原版二进制）
./cn.sh start | stop | restart | logs
./growth.sh ALL                   # 查活动参与：领猫/任务/体力（只读 dry-run）
cd international && ./run.sh      # 国际站（我们的二进制）
cd console && ./run.sh start      # 中控 → http://127.0.0.1:7860/?token=…
```

中控对 CN 只能启停/状态/日志/模型（原版无 /admin 运维接口，签到保活由其内部
调度器自动执行）；对国际站全部功能可用（启停/签到/保活/解冻）。

## 更新上游代码（两条独立动线）

**CN —— 零冲突跟进**（原版目录永远干净，pull 完重编重启即可）：

```bash
cd upstream && git pull
docker run --rm -v "$PWD":/src -v wb2api-gomod:/go/pkg/mod -v wb2api-gocache:/root/.cache/go-build \
  -w /src -e GOOS=darwin -e GOARCH=arm64 -e CGO_ENABLED=0 \
  golang:1.23-alpine go build -trimpath -ldflags="-s -w" -o /src/wb2api ./cmd/server
cd .. && ./cn.sh restart
```

**国际版 —— 定期合并**（要上游新功能时才做，有冲突要解）：

```bash
cd international
git fetch upstream
git merge upstream/master     # 解冲突 → 测试 → 重编 wb2api/console → 重启
git push fork master:feat/dual-region-web-console
```

重编国际版二进制（cmd 目录换 console 即编中控）：

```bash
docker run --rm -v "$PWD":/src -v wb2api-gomod:/go/pkg/mod -v wb2api-gocache:/root/.cache/go-build \
  -w /src -e GOOS=darwin -e GOARCH=arm64 -e CGO_ENABLED=0 \
  golang:1.23-alpine go build -trimpath -ldflags="-s -w" -o /src/wb2api ./cmd/server
```

中控二进制更新后拷到 `console/console`：`cp international/console console/console`。

## 端口与密钥

| 服务 | 端口 | 密钥位置 |
|---|---|---|
| CN（原版） | 7864 | `upstream/config.json` 的 `api_key` |
| 国际站 | 7865 | `international/config.json` 的 `instances.global.api_key` |
| 中控 | 7860 | `console/config.json` 的 `console.token`（访问用 `?token=`） |

账号凭证按区域分家：CN 3 个在 `upstream/auths/`，国际站 3 个在
`international/auths/`，登录对应目录里的 `login.sh`（CN 用 `upstream/login.sh`，
国际站用 `international/login.sh global`）。
