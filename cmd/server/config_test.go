package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 60 {
		t.Errorf("soft=%v", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// TestRegionDefaultBoth 默认（不写 region）= 两区混编，向后兼容旧配置。
func TestRegionDefaultBoth(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "" {
		t.Errorf("default region=%q want empty (both)", c.Region)
	}
}

// TestRegionFromFile region=cn / global 均接受，且大小写与空白被归一。
func TestRegionFromFile(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"cn"`, "cn"},
		{`"global"`, "global"},
		{`"  CN  "`, "cn"},
		{`"Global"`, "global"},
		{`""`, ""},
	} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"region":`+tc.in+`}`), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatalf("region=%s: %v", tc.in, err)
		}
		if c.Region != tc.want {
			t.Errorf("region=%s: got %q want %q", tc.in, c.Region, tc.want)
		}
	}
}

// TestRegionFromEnv WB2A_REGION 覆盖文件值。
func TestRegionFromEnv(t *testing.T) {
	t.Setenv("WB2A_REGION", "global")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "global" {
		t.Errorf("region=%q want global from env", c.Region)
	}
}

// TestRegionInvalid 非法值必须报错（防止写错一个字母就静默变成"两区混编"）。
func TestRegionInvalid(t *testing.T) {
	for _, bad := range []string{`"both"`, `"cnn"`, `"international"`, `"us"`} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"region":`+bad+`}`), 0o600)
		if _, err := Load(fp); err == nil {
			t.Errorf("region=%s: want error, got nil", bad)
		}
	}
}

func TestRegionInvalidEnv(t *testing.T) {
	t.Setenv("WB2A_REGION", "bogus")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for invalid WB2A_REGION")
	}
}

// --- 多实例（合并配置）格式 ---

const multiInstanceCfg = `{
  "api_key": "shared-key",
  "auth_dir": "./auths",
  "pool": {"max_in_flight": 7},
  "instances": {
    "cn": {
      "region": "cn",
      "listen": ":7864",
      "state_file": "./data/state.cn.json"
    },
    "global": {
      "region": "global",
      "listen": ":7865",
      "state_file": "./data/state.global.json",
      "api_key": "global-key",
      "pool": {"max_in_flight": 2}
    }
  }
}`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestInstanceInheritsShared 共享段字段被实例继承，实例只写差异。
func TestInstanceInheritsShared(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	c, err := LoadForInstance(fp, "cn")
	if err != nil {
		t.Fatal(err)
	}
	if c.InstanceName != "cn" {
		t.Errorf("InstanceName=%q want cn", c.InstanceName)
	}
	if c.APIKey != "shared-key" {
		t.Errorf("api_key=%q want shared-key (inherit)", c.APIKey)
	}
	if c.AuthDir != "./auths" {
		t.Errorf("auth_dir=%q want ./auths (inherit)", c.AuthDir)
	}
	if c.Pool.MaxInFlight != 7 {
		t.Errorf("max_in_flight=%d want 7 (inherit)", c.Pool.MaxInFlight)
	}
	if c.Region != "cn" || c.Listen != ":7864" || c.StateFile != "./data/state.cn.json" {
		t.Errorf("instance fields=%q/%q/%q", c.Region, c.Listen, c.StateFile)
	}
}

// TestInstanceOverrides 实例分节覆盖同名共享字段（含嵌套结构体里的字段）。
func TestInstanceOverrides(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	c, err := LoadForInstance(fp, "global")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "global-key" {
		t.Errorf("api_key=%q want global-key (override)", c.APIKey)
	}
	if c.Pool.MaxInFlight != 2 {
		t.Errorf("max_in_flight=%d want 2 (override)", c.Pool.MaxInFlight)
	}
	// 未覆盖的字段仍继承共享段。
	if c.AuthDir != "./auths" {
		t.Errorf("auth_dir=%q want ./auths (inherit)", c.AuthDir)
	}
	if c.Region != "global" || c.Listen != ":7865" {
		t.Errorf("region=%q listen=%q", c.Region, c.Listen)
	}
}

// TestInstancesClearedAfterLoad instances 分节不残留到运行期配置。
func TestInstancesClearedAfterLoad(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	c, err := LoadForInstance(fp, "cn")
	if err != nil {
		t.Fatal(err)
	}
	if c.Instances != nil {
		t.Errorf("Instances should be nil after load, got %d entries", len(c.Instances))
	}
}

// TestMultiInstanceRequiresName 多实例配置不给 -instance 必须报错（而不是跑个空壳）。
func TestMultiInstanceRequiresName(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	_, err := LoadForInstance(fp, "")
	if err == nil {
		t.Fatal("want error when instances present but instance name missing")
	}
	for _, want := range []string{"cn", "global", "-instance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestUnknownInstance 名字写错时报错并列出可选实例。
func TestUnknownInstance(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	_, err := LoadForInstance(fp, "cnd")
	if err == nil {
		t.Fatal("want error for unknown instance")
	}
	if !strings.Contains(err.Error(), "cn") || !strings.Contains(err.Error(), "global") {
		t.Errorf("error %q should list available instances", err)
	}
}

// TestLegacySingleInstanceStillWorks 旧的单实例格式：不带 -instance 照常加载。
func TestLegacySingleInstanceStillWorks(t *testing.T) {
	fp := writeCfg(t, `{"listen":":9999","api_key":"k","region":"cn"}`)
	c, err := LoadForInstance(fp, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" || c.Region != "cn" {
		t.Errorf("c=%+v", c)
	}
	if c.InstanceName != "" {
		t.Errorf("InstanceName=%q want empty for legacy config", c.InstanceName)
	}
	// 显式传名字也不报错（脚本统一传参的场景），仅记下名字。
	c2, err := LoadForInstance(fp, "cn")
	if err != nil {
		t.Fatal(err)
	}
	if c2.InstanceName != "cn" || c2.Listen != ":9999" {
		t.Errorf("c2=%+v", c2)
	}
}

// TestInstanceEnvWins env 覆盖优先级最高（在实例覆盖之上）。
func TestInstanceEnvWins(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":1234")
	fp := writeCfg(t, multiInstanceCfg)
	c, err := LoadForInstance(fp, "cn")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":1234" {
		t.Errorf("listen=%q want env :1234", c.Listen)
	}
}

// TestInstanceNames 实例名排序返回；单实例格式返回空。
func TestInstanceNames(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	names, err := InstanceNames(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "cn" || names[1] != "global" {
		t.Errorf("names=%v want [cn global]", names)
	}

	legacy := writeCfg(t, `{"listen":":7863"}`)
	names, err = InstanceNames(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("legacy config should yield no names, got %v", names)
	}
}

func TestInstanceNamesMissingFile(t *testing.T) {
	if _, err := InstanceNames(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("want error for missing config")
	}
}

// TestListInstances 返回各实例的生效监听地址（走完整合并逻辑）。
func TestListInstances(t *testing.T) {
	fp := writeCfg(t, multiInstanceCfg)
	out, err := ListInstances(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("out=%v want 2 instances", out)
	}
	if out[0].Name != "cn" || out[0].Listen != ":7864" {
		t.Errorf("out[0]=%+v", out[0])
	}
	if out[1].Name != "global" || out[1].Listen != ":7865" {
		t.Errorf("out[1]=%+v", out[1])
	}
}
