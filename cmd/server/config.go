// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// Config 顶层配置。
//
// 支持两种文件格式（自动识别，无需声明）：
//
//  1. **多实例格式**（推荐）：一份 config.json 描述全部实例，共享段 + instances 分节。
//     启动时用 -instance <name> 指定要跑哪个：
//
//     {
//     "api_key": "sk-...",                  // 共享：未在实例里覆盖的字段都继承这里
//     "auth_dir": "./auths",
//     "console": { "listen": "127.0.0.1:7860", "token": "..." },
//     "instances": {
//     "cn":     { "region": "cn",     "listen": ":7864", "state_file": "./data/state.cn.json" },
//     "global": { "region": "global", "listen": ":7865", "state_file": "./data/state.global.json" }
//     }
//     }
//
//  2. **单实例格式**（旧，仍兼容）：顶层直接写 listen/region/...，不带 instances。
//     此时 -instance 可省略（给了也只会校验名字存在与否）。
//
// 合并规则：先取 Default() → 覆盖顶层共享字段 → 再用 instances[name] 覆盖 → env 覆盖。
// 因此共享段写一次，各实例只写差异（通常就是 region / listen / state_file）。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// Region 限定本实例只服务某一区域的账号（"cn" / "global"）。
	// 空 = 不限定，两区账号混编同池（向后兼容旧配置）。
	// 权威依据是凭证的 domain（auth.Region()）；被滤掉的账号不会进池，
	// 因此本实例只会向该区域的上游 host 发请求。
	Region string `json:"region"`

	// Console 控制台相关配置（由 cmd/console 读取；server 忽略）。
	// 放进同一份 config 是为了"一个文件管全部"，省掉控制台单独传 -token。
	Console struct {
		// Listen 控制台监听地址。空 → cmd/console 用其默认（127.0.0.1:7860）。
		Listen string `json:"listen"`
		// Token 控制台访问令牌。空 = 不鉴权（仅在监听回环时允许）。
		Token string `json:"token"`
	} `json:"console"`

	// Instances 多实例分节：key 为实例名（-instance 取值），value 为该实例的覆盖字段（原始 JSON）。
	// 用 RawMessage 而非具体结构体，是为了让"实例覆盖"复用同一套字段解析——把该实例的
	// JSON 再 Unmarshal 到已载入共享字段的 Config 上，天然实现"只覆盖出现过的字段"，
	// 不必为每个字段维护指针类型。仅用于多实例格式；单实例格式下为空。
	Instances map[string]json.RawMessage `json:"instances"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
	} `json:"schedule"`

	// ModelsExtra 额外对外暴露的模型 ID（追加到上游动态列表之后，去重）。
	//
	// 为什么需要它：上游的模型清单本身不准确，实测两个方向都有偏差——
	//   - 上游 catalog（/v3/config 的 data.models）会列出本区**不存在**的模型
	//     （如国际站列出 gpt-5.1-codex / gemini-2.5-flash，调用返回 code=11102）；
	//   - 而实际**可用**的模型又可能不在任何列表里（如国际站的 deepseek-v4.1-flash、
	//     hy4-preview：/v3/config 全文搜索 0 次，但 chat 调用正常返回）。
	// 官方客户端的模型白名单（agents 里 name=="cli" 的 models）同样既不充分也不完备。
	//
	// 因此：以官方白名单为默认来源，再给一个由使用者裁定的补充入口。
	// 这里填的 ID 只影响 /v1/models 的展示与模型→区域亲和判断，不影响能否调用
	// （/v1/chat/completions 对 model 字段本身不做白名单校验）。
	ModelsExtra []string `json:"models_extra"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	InstanceName        string        `json:"-"` // 本次运行的实例名（多实例格式下由 -instance 决定）
	SoftRateDur         time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。instance 为空时按单实例格式处理。
func Load(path string) (*Config, error) {
	return LoadForInstance(path, "")
}

// LoadForInstance 载入配置并合并指定实例的覆盖字段。
//
// 合并顺序：Default() → 文件顶层（共享段）→ instances[instance] → 环境变量 → normalize。
// instance 为空时要求文件是单实例格式（无 instances 分节）；若文件是多实例格式而没给
// instance，直接报错并列出可选名字——避免"以为在跑某个实例、实际跑了个空壳"。
func LoadForInstance(path, instance string) (*Config, error) {
	c := Default()
	var raw []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		raw = b
		// 顶层：共享字段 + instances 分节（RawMessage 先存着，稍后再按名字解）
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	if len(c.Instances) > 0 {
		if instance == "" {
			return nil, fmt.Errorf("配置含 %d 个实例 %v，必须用 -instance 指定要运行哪个",
				len(c.Instances), instanceNames(c.Instances))
		}
		body, ok := c.Instances[instance]
		if !ok {
			return nil, fmt.Errorf("实例 %q 不在配置里（可选：%v）", instance, instanceNames(c.Instances))
		}
		// 把该实例的覆盖字段叠到已载入共享字段的 Config 上：
		// 只覆盖 JSON 里出现过的键，其余继承共享段。
		if err := json.Unmarshal(body, c); err != nil {
			return nil, fmt.Errorf("parse instances.%s: %w", instance, err)
		}
		c.InstanceName = instance
	} else if instance != "" {
		// 单实例格式但显式指定了名字：不报错（便于脚本统一传参），只记下名字。
		c.InstanceName = instance
	}

	// instances 分节本身不参与后续逻辑，清掉避免误用。
	c.Instances = nil

	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// instanceNames 返回排序后的实例名（报错信息用，保证输出稳定）。
func instanceNames(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// InstanceBrief 脚本枚举用：实例名 + 生效监听地址。
type InstanceBrief struct {
	Name   string
	Listen string
}

// ListInstances 返回各实例的生效监听地址（按名字排序），走完整合并逻辑
// （共享段 + instances[name] + env 覆盖），因此与实例真正启动时用的地址一致。
// 单实例格式（无 instances 分节）返回空列表。
func ListInstances(path string) ([]InstanceBrief, error) {
	names, err := InstanceNames(path)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceBrief, 0, len(names))
	for _, n := range names {
		c, err := LoadForInstance(path, n)
		if err != nil {
			return nil, err
		}
		out = append(out, InstanceBrief{Name: n, Listen: c.Listen})
	}
	return out, nil
}

// InstanceNames 只读配置文件里的实例名（排序），供外部脚本/容器入口枚举要启动的实例。
// 单实例格式（无 instances 分节）返回空列表——调用方据此走"不带 -instance"的路径。
func InstanceNames(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var probe struct {
		Instances map[string]json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return instanceNames(probe.Instances), nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_REGION"); v != "" {
		c.Region = v
	}
	// 逗号分隔的补充模型列表；空串不改动（与其他 env 的"非空才覆盖"语义一致）。
	if v := os.Getenv("WB2A_MODELS_EXTRA"); v != "" {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		c.ModelsExtra = out
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// region：空 = 两区混编（默认，向后兼容）；非空必须是 cn / global。
	// 不接受 "both" 之类的别名——空串已是"不限定"的表达，多一种写法只会引入歧义。
	c.Region = strings.ToLower(strings.TrimSpace(c.Region))
	if c.Region != "" && c.Region != auth.RegionCN && c.Region != auth.RegionGlobal {
		return fmt.Errorf("region must be %q, %q or empty (both), got %q",
			auth.RegionCN, auth.RegionGlobal, c.Region)
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return nil
}
