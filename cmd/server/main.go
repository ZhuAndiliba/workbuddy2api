// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// regionLabel 仅用于日志展示：空 region 表示两区混编。
func regionLabel(region string) string {
	if region == "" {
		return "both"
	}
	return region
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	instance := flag.String("instance", "", "多实例配置下要运行的实例名（见 config 的 instances 分节）")
	listInstances := flag.Bool("list-instances", false, "只打印 config 里的实例名（每行一个）后退出；供脚本/容器入口枚举")
	printListen := flag.Bool("print-listen", false, "只打印本次实例生效的监听地址后退出；供脚本读配置")
	flag.Parse()

	// 枚举模式：只读配置、不启服务，让 shell 侧不必自己解析 JSON。
	if *listInstances {
		names, err := InstanceNames(*cfgPath)
		if err != nil {
			log.Fatalf("%v", err)
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return
	}
	if *printListen {
		c, err := LoadForInstance(*cfgPath, *instance)
		if err != nil {
			log.Fatalf("%v", err)
		}
		fmt.Println(c.Listen)
		return
	}

	cfg, err := LoadForInstance(*cfgPath, *instance)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = LoadForInstance("", *instance)
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	// 一区一实例：region 非空时只保留该区账号，其余不进池（避免打往错误 host）。
	if cfg.Region != "" {
		total := len(auths)
		auths = auth.FilterRegion(auths, cfg.Region)
		log.Printf("region=%s: kept %d/%d account(s) from %s (skipped %d by region)",
			cfg.Region, len(auths), total, cfg.AuthDir, total-len(auths))
	} else {
		log.Printf("loaded %d account(s) from %s (region=both)", len(auths), cfg.AuthDir)
	}

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	// 按区域加键命名空间，使两区实例共用一个 Redis 时互不覆盖。
	store := redisstore.NewWithRegion(cfg.Upstash.URL, cfg.Upstash.Token, cfg.Region)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 上游模型清单不准，允许配置补充（见 config.go 的 ModelsExtra 注释）。
	up.ModelsExtra = cfg.ModelsExtra
	if len(cfg.ModelsExtra) > 0 {
		log.Printf("models_extra: %d 个补充模型已注入 %v", len(cfg.ModelsExtra), cfg.ModelsExtra)
	}

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Region:       cfg.Region,
		// 手动触发定时任务（网页控制台用）：直接复用调度器的同一实现，
		// 避免"手动签到"与"定时签到"两条路径行为漂移。
		OnCheckin:   sch.RunCheckinNow,
		OnKeepalive: sch.RunKeepaliveNow,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// 启动日志带上实例名（多实例格式下便于从 docker logs 区分是哪一份配置）。
	if cfg.InstanceName != "" {
		log.Printf("workbuddy2api listening on %s (instance=%s, api_key=%v, region=%s)",
			cfg.Listen, cfg.InstanceName, cfg.APIKey != "", regionLabel(cfg.Region))
	} else {
		log.Printf("workbuddy2api listening on %s (api_key=%v, region=%s)", cfg.Listen, cfg.APIKey != "", regionLabel(cfg.Region))
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
