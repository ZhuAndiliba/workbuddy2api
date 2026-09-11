// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// Region 本实例限定的区域（"cn" / "global"；空 = 两区混编）。仅作观测与
	// 区域偏好兜底，账号过滤在启动时（auth.FilterRegion）已完成。
	Region string
	// OnCheckin / OnKeepalive 手动触发定时任务（网页控制台用）。
	// nil 时对应端点返回 501，便于只装配 HTTP 层的测试与裁剪部署。
	OnCheckin   func()
	OnKeepalive func()
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 运维面板（静态壳，无鉴权；数据仍走上面各鉴权接口）+ 根路径跳转。
	h.mux.HandleFunc("GET /ui", h.ui)
	h.mux.HandleFunc("GET /{$}", h.root)
	// 账号池运维动作（走 withAuth，与 /status 同级别的保护）。
	// 这些会真实触发上游调用或改动池状态，因此不放无鉴权路径。
	h.mux.HandleFunc("POST /admin/checkin", h.withAuth(h.adminCheckin))
	h.mux.HandleFunc("POST /admin/keepalive", h.withAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/revive", h.withAuth(h.adminRevive))
	return h
}

// adminCheckin 立即对全部账号执行签到 + 余额刷新（复用定时任务同一实现）。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.OnCheckin == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "message": "本实例未装配签到能力"})
		return
	}
	n, _, _, _, _ := h.cfg.Pool.CountsDetailed()
	h.cfg.OnCheckin()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": fmt.Sprintf("已对 %d 个账号执行签到 + 余额刷新", n),
	})
}

// adminKeepalive 立即刷新全部账号 token（复用定时任务同一实现）。
func (h *Handler) adminKeepalive(w http.ResponseWriter, r *http.Request) {
	if h.cfg.OnKeepalive == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "message": "本实例未装配保活能力"})
		return
	}
	total, _, _, _, _ := h.cfg.Pool.CountsDetailed()
	h.cfg.OnKeepalive()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": fmt.Sprintf("已对 %d 个账号刷新 token", total),
	})
}

// adminRevive 手动解冻：body {"uid":"..."} 解冻单个，空 body 或 {} 解冻全部。
// 只清冷却，不碰熔断与 disabled（语义同"签到解冻"）。
func (h *Handler) adminRevive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	// 允许空 body：读失败就当"解冻全部"。
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)

	if req.UID == "" {
		n := h.cfg.Pool.ReviveAll()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "message": fmt.Sprintf("已解冻 %d 个账号的冷却", n),
		})
		return
	}
	if !h.cfg.Pool.Revive(req.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "账号不存在: " + req.UID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已解冻 " + shortUID(req.UID)})
}

// shortUID 仅用于提示文案，避免把完整 uid 反复打到前端。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"healthy": healthy, "total": total})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	instanceRegion := h.cfg.Region
	if instanceRegion == "" {
		instanceRegion = "both"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		"region":          instanceRegion,
	})
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存（按区域分桶）。
// 各区域上游模型表不同（CN 与 global 是两套部署），必须分区缓存，
// 否则一个区的成功拉取会覆盖另一个区的列表。
type modelsBucket struct {
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

var dynamicModelsCache struct {
	sync.RWMutex
	byRegion map[string]*modelsBucket
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 汇总各区域动态模型（含 context_length）；全部区域都拿不到时回退静态表。
// 多区域账号池下取并集：客户端只关心"这个网关能跑哪些模型"，不必区分账号来自哪个区。
func (h *Handler) modelList() []map[string]any {
	regions := h.cfg.Pool.Regions()
	if len(regions) == 0 {
		return staticModels
	}
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(staticModels))
	for _, region := range regions {
		for _, mi := range h.fetchDynamicModels(region) {
			if mi.ID == "" || seen[mi.ID] {
				continue
			}
			seen[mi.ID] = true
			out = append(out, modelEntry(mi))
		}
	}
	if len(out) == 0 {
		return staticModels
	}
	return out
}

// modelEntry 把单个上游模型信息包装成 OpenAI 模型对象。
func modelEntry(mi upstream.ModelInfo) map[string]any {
	ctx := mi.ContextWindow
	if ctx == 0 {
		ctx = 131072 // 兜底
	}
	return map[string]any{
		"id":                mi.ID,
		"object":            "model",
		"created":           1753600000,
		"owned_by":          "workbuddy",
		"context_length":    ctx,
		"max_output_tokens": mi.MaxTokens,
	}
}

// fetchDynamicModels 从指定区域的任一健康账号拉模型列表（含 contextWindow/maxTokens），按区缓存 1h。
// 拉取失败记录时间戳进入该区 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels(region string) []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	b := dynamicModelsCache.byRegion[region]
	dynamicModelsCache.RUnlock()
	if b != nil {
		if len(b.ids) > 0 && time.Since(b.fetched) < dynamicModelsTTL {
			return b.ids
		}
		// 失败负缓存：冷却期内不再请求上游。
		if !b.lastFail.IsZero() && time.Since(b.lastFail) < modelsFetchFailCooldown {
			return nil
		}
	}

	acct := h.cfg.Pool.PickExcludingInRegion(region, nil)
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持本区负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		h.storeModelsBucket(region, nil, time.Now())
		return nil
	}
	h.storeModelsBucket(region, infos, time.Time{})
	return infos
}

// storeModelsBucket 写入某区域的模型缓存（lastFail 非零表示失败负缓存，成功时清空）。
func (h *Handler) storeModelsBucket(region string, infos []upstream.ModelInfo, lastFail time.Time) {
	dynamicModelsCache.Lock()
	defer dynamicModelsCache.Unlock()
	if dynamicModelsCache.byRegion == nil {
		dynamicModelsCache.byRegion = map[string]*modelsBucket{}
	}
	b := dynamicModelsCache.byRegion[region]
	if b == nil {
		b = &modelsBucket{}
		dynamicModelsCache.byRegion[region] = b
	}
	if lastFail.IsZero() {
		b.ids = infos
		b.fetched = time.Now()
		b.lastFail = time.Time{}
		return
	}
	b.lastFail = lastFail
}

// preferredRegion 返回该模型唯一可用的区域；无法判定（模型为空 / 各区都有 / 各区都没有）时返回空串。
// 多区域账号池下用它把请求直接落到对的区，省掉"发错区 → 上游拒 → 换号"的一轮浪费。
// 只在已有成功拉取缓存的区域间比较，缓存未就绪时返回空串（退回全区轮换，行为不变）。
func (h *Handler) preferredRegion(model string) string {
	// 实例已限定区域：池里只有该区账号，无需（也不该）再按模型推断区域。
	if h.cfg.Region != "" {
		return ""
	}
	if model == "" {
		return ""
	}
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	owner, found := "", false
	for region, b := range dynamicModelsCache.byRegion {
		if len(b.ids) == 0 {
			continue
		}
		has := false
		for _, mi := range b.ids {
			if mi.ID == model {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		if found {
			return "" // 多个区都有 → 无需偏好
		}
		owner, found = region, true
	}
	return owner
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 模型 → 区域偏好：该模型只在某一个区存在时（如仅 CN 提供的模型），
	// 把选号限定在该区，省掉"发错区 → 上游拒 → 换号"的整轮浪费。
	// 单区域池 / 缓存未就绪 / 各区都有 → 空串，行为与改造前一致。
	preferRegion := h.preferredRegion(parseModelFromBody(body))

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID)
			if acct != nil && preferRegion != "" && acct.Region() != preferRegion {
				// 粘性号区域与该模型不匹配（如会话先在 CN 号上建立，之后换了仅国际站提供的模型）：
				// 解绑后改走偏好区，避免用错区的号白跑一轮。
				// 注意：PickByUID 只记 lastUsed、不占在途租约（租约由下方 Acquire 拿），故此处不能 Release。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
				acct = nil
			} else if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcludingInRegion(preferRegion, tried)
			if acct == nil && preferRegion != "" {
				// 偏好区无可用号时放宽到全区：模型表可能滞后或不全，
				// 不能仅凭"该模型只在这个区"就把请求锁死成 503。
				acct = h.cfg.Pool.PickExcluding(tried)
			}
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 五条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate / ErrNotFound → Cooldown(CoolSoft)：即时软冷却（429/404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
