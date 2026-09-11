// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义，实测国际站以 HTTP 200 + code=14003
// "too many requests" 返回；此类响应若不识别，账号既不被冷却也不喂熔断，
// 下次请求仍会被选中，表现为"反复失败但不换号"。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown 后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//  2. sessionDeadMarkers —— 需人工重登的终态。若 401 body 同时含 12153 与限流文案，
//     归 session_dead：短冷却救不活失效 session，误判为限流会让死号留在池中反复被选中；
//     且此层 marker（12153 等）比限流层的大范围子串更具体，具体优先于宽泛。
//  3. softRateMarkers —— 非 429 状态码携带限流文案（HTTP 200 + code=14003 等）。
//  4. status==429 —— body 无文案时的兜底识别。
//  5. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额/限流关键词的情况已被上面两层捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按区域分桶：同名模型在不同区域的受支持档位可能不同，混用会导致错误的降级结果。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string // region -> model -> supportedEfforts

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// ModelsExtra 额外对外暴露的模型 ID（追加到动态列表之后，去重）。
	// 用途见 cmd/server/config.go 的 ModelsExtra 注释：上游清单本身不准，
	// 需要给使用者一个补充入口。仅影响 /v1/models 的展示，不影响调用可行性。
	ModelsExtra []string

	ChatBaseCN      string
	BillingBaseCN   string
	ChatBaseGlobal  string
	BillingBaseGlob string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		// 国际站（domain 为 *.workbuddy.ai / *.codebuddy.ai 的账号）走同一套 host：
		// 两者是同一后端部署（同 Keycloak realm、同 /v2/plugin/* 端点族，实测互认 Origin）。
		ChatBaseGlobal:  "https://www.workbuddy.ai",
		BillingBaseGlob: "https://www.workbuddy.ai",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// chatBase 返回账号所属区域的上游 host；nil / 未知 region 一律回落 CN。
func (c *Client) chatBase(a *auth.Auth) string {
	if a.Region() == auth.RegionGlobal {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// 按账号所属区域取 effort 能力表，避免跨区同名模型混用；
// 国际站额外要求首条消息必须是 system（否则 code=11128），此处自动补齐。
func (c *Client) prepareBody(a *auth.Auth, body []byte) []byte {
	region := a.Region()
	return PrepareBodyOptFull(body, c.SanitizeFingerprints, c.effortsSnapshot(region), region == auth.RegionGlobal)
}

// effortsSnapshot 返回指定区域 effort 能力缓存的副本；该区无缓存或为空时返回 nil（未知 → 透传不降级）。
func (c *Client) effortsSnapshot(region string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	byModel := c.efforts[region]
	if len(byModel) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(byModel))
	for k, v := range byModel {
		cp[k] = v
	}
	return cp
}

// billingBase 返回账号所属区域的计费 host；nil / 未知 region 一律回落 CN。
func (c *Client) billingBase(a *auth.Auth) string {
	if a.Region() == auth.RegionGlobal {
		return c.BillingBaseGlob
	}
	return c.BillingBaseCN
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(a, body)))
	if err != nil {
		return nil, 0, nil, err
	}
	ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
}

// FetchModels 调上游动态模型接口。
// 主路径 /v3/config（官方客户端同款，CN 与国际站都可用，字段最全）；
// 失败时回落旧路径 /console/enterprises/personal/models（仅 CN 可用，国际站返回 500）。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	out, err := c.fetchModelsAny(a)
	if err != nil {
		return nil, err
	}
	out = mergeModelsExtra(out, c.ModelsExtra)
	c.cacheEfforts(a.Region(), out)
	return out, nil
}

// mergeModelsExtra 把配置里补充的模型 ID 追加到动态列表之后（按 ID 去重，保持原顺序）。
// 只补 ID 不带参数：这些模型不在上游清单里，拿不到 maxInputTokens 等元数据，
// 主动留空由上层兜底，好过凭空编一个。
func mergeModelsExtra(base []ModelInfo, extra []string) []ModelInfo {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	for _, m := range base {
		seen[m.ID] = true
	}
	out := base
	for _, id := range extra {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id})
	}
	return out
}

// fetchModelsAny 先试 /v3/config，失败回落旧 models 接口。
func (c *Client) fetchModelsAny(a *auth.Auth) ([]ModelInfo, error) {
	v3Out, v3Err := c.fetchModelsV3(a)
	if v3Err == nil {
		return v3Out, nil
	}
	// 保留旧路径作为兜底：/v3/config 若在某区被下线，仍能退化到旧接口。
	if legacyOut, legacyErr := c.fetchModelsLegacy(a); legacyErr == nil {
		return legacyOut, nil
	}
	return nil, v3Err
}

// cacheEfforts 刷新指定区域的 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
// 空缓存也写入：表示"该区已拉取但无 effort 信息"，与"该区从未拉取"（无键 → 返回 nil）区分开。
func (c *Client) cacheEfforts(region string, out []ModelInfo) {
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = map[string]map[string][]string{}
	}
	c.efforts[region] = cache
	c.effortsMu.Unlock()
}

// modelsEnvelope /v3/config 与旧 models 接口共用的信封结构。
// cliModels：从 agents 里取 name=="cli" 的 models 列表作为"对外可用模型"白名单。
// 注意 /v3/config 无 disabled 字段，缺省即视为可用。
type modelsEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Models []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			MaxInputTokens  int64  `json:"maxInputTokens"`
			MaxOutputTokens int64  `json:"maxOutputTokens"`
			Disabled        bool   `json:"disabled"`
			Reasoning       struct {
				Effort           string   `json:"effort"`
				SupportedEfforts []string `json:"supportedEfforts"`
			} `json:"reasoning"`
		} `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	} `json:"data"`
}

// fetchModelsV3 走 /v3/config（官方客户端的数据源，两区通用）。
func (c *Client) fetchModelsV3(a *auth.Auth) ([]ModelInfo, error) {
	raw, err := c.getAuthed(a, "/v3/config")
	if err != nil {
		return nil, err
	}
	return parseModels(raw, "/v3/config")
}

// fetchModelsLegacy 走旧接口 /console/enterprises/personal/models（CN 可用）。
func (c *Client) fetchModelsLegacy(a *auth.Auth) ([]ModelInfo, error) {
	raw, err := c.getAuthed(a, "/console/enterprises/personal/models")
	if err != nil {
		return nil, err
	}
	return parseModels(raw, "models api")
}

// getAuthed 发一个带账号鉴权头的 GET，返回响应体（1 MiB 上限）。
func (c *Client) getAuthed(a *auth.Auth, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s status %d: %s", path, resp.StatusCode, truncate(string(raw), 120))
	}
	return raw, nil
}

// parseModels 解析模型信封，并按 cli agent 的 models 白名单过滤。
// src 仅用于错误信息（区分是哪个路径失败的）。
func parseModels(raw []byte, src string) ([]ModelInfo, error) {
	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s parse: %w", src, err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s code=%d", src, env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("%s: no cli agent models found", src)
	}
	type modelRow struct {
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
	}
	dynMap := make(map[string]modelRow, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = modelRow{m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s returned empty list", src)
	}
	return out, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSON(req)
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
