package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortOf(t *testing.T) {
	cases := map[string]string{
		":7864":          "7864",
		"127.0.0.1:7865": "7865",
		"0.0.0.0:7863":   "7863",
		"[::1]:7866":     "7866",
		"7867":           "7867",
		"  :7868  ":      "7868",
		"":               "",
		"localhost:":     "",
	}
	for in, want := range cases {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q)=%q want %q", in, got, want)
		}
	}
}

// TestIsLoopbackHost 决定"是否允许不带 token 启动"——判错就是把进程控制面暴露出去。
func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{
		"127.0.0.1:7860", "localhost:7860", "[::1]:7860",
		"127.0.0.2:7860", "127.1.2.3:7860", "127.0.0.1",
	}
	for _, l := range loopback {
		if !isLoopbackHost(l) {
			t.Errorf("isLoopbackHost(%q)=false want true", l)
		}
	}
	public := []string{
		"0.0.0.0:7860", ":7860", "192.168.1.5:7860", "10.0.0.1:7860",
		"example.com:7860", "[::]:7860",
	}
	for _, l := range public {
		if isLoopbackHost(l) {
			t.Errorf("isLoopbackHost(%q)=true want false（对非本机可见必须要求 token）", l)
		}
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "x.log")
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, "line"+itoa(i))
	}
	if err := os.WriteFile(fp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(fp, 5)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	want := "line96\nline97\nline98\nline99\nline100"
	if got != want {
		t.Errorf("tailFile(5)=%q want %q", got, want)
	}
	// 请求行数超过文件行数时返回全部，不报错。
	got, err = tailFile(fp, 5000)
	if err != nil {
		t.Fatalf("tailFile(5000): %v", err)
	}
	if len(strings.Split(got, "\n")) != 100 {
		t.Errorf("tailFile(5000) 行数=%d want 100", len(strings.Split(got, "\n")))
	}
}

func TestTailFileMissing(t *testing.T) {
	if _, err := tailFile(filepath.Join(t.TempDir(), "nope.log"), 10); err == nil {
		t.Fatal("want error for missing file")
	}
}

// TestAuthorized token 为空时放行；设了 token 后必须匹配头或查询参数。
func TestAuthorized(t *testing.T) {
	s := &Server{}

	// 未设 token：一律放行（绑定回环时的默认模式）。
	req := mustReq(t, "/api/state")
	if !s.authorized(req) {
		t.Error("empty token should allow")
	}

	s.token = "s3cret"
	if s.authorized(req) {
		t.Error("token set: request without token must be rejected")
	}
	req = mustReq(t, "/api/state")
	req.Header.Set("X-Console-Token", "s3cret")
	if !s.authorized(req) {
		t.Error("matching header should allow")
	}
	req = mustReq(t, "/api/state")
	req.Header.Set("X-Console-Token", "wrong")
	if s.authorized(req) {
		t.Error("wrong header must be rejected")
	}
	req = mustReq(t, "/api/state?token=s3cret")
	if !s.authorized(req) {
		t.Error("matching query token should allow")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func mustReq(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// TestTailFileDropsPartialFirstLine 从文件中段截取时，首行是半截的，必须丢掉。
// 否则大日志（超过 logTailBytes）展示出来的第一行会是残句。
func TestTailFileDropsPartialFirstLine(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "big.log")
	// 造一个超过 logTailBytes 的文件：每行 "LINE-<i>-xxxxxxxx..."，总长 > 256KiB。
	var b strings.Builder
	for i := 0; b.Len() < logTailBytes+50_000; i++ {
		b.WriteString("LINE-")
		b.WriteString(itoa(i))
		b.WriteString("-")
		b.WriteString(strings.Repeat("x", 100))
		b.WriteString("\n")
	}
	if err := os.WriteFile(fp, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := tailFile(fp, 3)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), got)
	}
	// 每一行都必须是完整行（以 LINE- 开头），不得出现半截残句。
	for i, l := range lines {
		if !strings.HasPrefix(l, "LINE-") {
			t.Errorf("line %d is a partial fragment: %q", i, l)
		}
		if !strings.HasSuffix(l, strings.Repeat("x", 100)) {
			t.Errorf("line %d is truncated: %q", i, l)
		}
	}
}

// TestRunningUsesReachability 容器部署下控制台看不到容器内的 pid 文件，
// 必须靠 /healthz 可达性判定"在跑"，否则会误报已停止、按钮状态错乱。
func TestRunningUsesReachability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 = 活着但无可用账号
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.test.json")
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	if err := os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:`+port+`","api_key":""}`), 0o600); err != nil {
		t.Fatal(err)
	}

	in := &instance{Name: "test", Label: "Test", Config: "config.test.json", root: dir}
	// 没有 pid 文件（模拟容器内），但 /healthz 可达 → 应判为在跑。
	ok, pid := in.running()
	if !ok {
		t.Error("reachable instance must be reported as running even without a visible pid file")
	}
	if pid != 0 {
		t.Errorf("pid=%d want 0 (not managed by this process)", pid)
	}

	// 端口关掉后应判为已停止。
	srv.Close()
	in2 := &instance{Name: "test2", Label: "Test2", Config: "config.test.json", root: dir}
	if ok, _ := in2.running(); ok {
		t.Error("unreachable instance with no pid file must be reported as stopped")
	}
}

// TestReachableAccepts503 503（进程活着但账号全不可用）也算可达。
func TestReachableAccepts503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	dir := t.TempDir()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	os.WriteFile(filepath.Join(dir, "config.x.json"),
		[]byte(`{"listen":"127.0.0.1:`+port+`"}`), 0o600)
	in := &instance{Name: "x", Config: "config.x.json", root: dir}
	if !in.reachable() {
		t.Error("503 must count as reachable (process is alive)")
	}
}

// TestInstanceNamesFromConfig 受管实例清单可从 config 的 instances 分节推导（排序）。
func TestInstanceNamesFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"listen":":7864","instances":{
		"global":{"listen":":7865"},
		"cn":{"listen":":7864"}
	}}`), 0o600)
	got, ok := instanceNames(cfg)
	if !ok {
		t.Fatal("ok=false for a readable config")
	}
	if len(got) != 2 || got[0] != "cn" || got[1] != "global" {
		t.Errorf("instanceNames=%v want [cn global]", got)
	}
}

// TestInstanceNamesLegacyConfig 单实例格式（无 instances 分节）：ok=true 但没有名字，
// 调用方据此只挂一个实例（而不是摆出两个指向同一进程的卡片）。
func TestInstanceNamesLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"listen":":7864"}`), 0o600)
	got, ok := instanceNames(cfg)
	if !ok {
		t.Error("ok=false for a readable single-instance config")
	}
	if len(got) != 0 {
		t.Errorf("instanceNames=%v want empty for single-instance config", got)
	}

	// 配置读不出来：ok=false，调用方才回落到内置默认实例名。
	if _, ok := instanceNames(filepath.Join(dir, "nope.json")); ok {
		t.Error("ok=true for a missing config")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{not json`), 0o600)
	if _, ok := instanceNames(bad); ok {
		t.Error("ok=true for an unparsable config")
	}
}

// TestLabelOf 展示名映射：已知实例给中文名，其余用实例名本身。
func TestLabelOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"cn", "CN"},
		{"global", "国际站"},
		{"edge", "edge"},
	} {
		if got := labelOf(tc.in); got != tc.want {
			t.Errorf("labelOf(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// TestDeriveInstances 中控 config 的 instances 分节可带各自 root/label：
// 绝对路径原样用，相对路径相对控制台 root 解析，缺省回落控制台 root 与 labelOf。
func TestDeriveInstances(t *testing.T) {
	consoleRoot := t.TempDir()
	other := t.TempDir()
	cfg := filepath.Join(consoleRoot, "config.json")
	os.WriteFile(cfg, []byte(`{
		"console": {"listen": "127.0.0.1:7860", "token": "t"},
		"instances": {
			"global": {"root": "`+other+`", "label": "国际站"},
			"cn":     {"root": "sub/dir"},
			"solo":   {}
		}
	}`), 0o600)

	insts := deriveInstances(cfg, consoleRoot)
	if len(insts) != 3 {
		t.Fatalf("instances=%d want 3", len(insts))
	}
	byName := map[string]*instance{}
	for _, in := range insts {
		byName[in.Name] = in
	}
	if got := byName["global"].root; got != other {
		t.Errorf("global root=%q want %q (absolute)", got, other)
	}
	if byName["global"].Label != "国际站" {
		t.Errorf("global label=%q", byName["global"].Label)
	}
	if got := byName["cn"].root; got != filepath.Join(consoleRoot, "sub/dir") {
		t.Errorf("cn root=%q want relative resolved against console root", got)
	}
	if byName["cn"].Label != "CN" {
		t.Errorf("cn label=%q want labelOf fallback", byName["cn"].Label)
	}
	if got := byName["solo"].root; got != consoleRoot {
		t.Errorf("solo root=%q want console root fallback", got)
	}
	if byName["solo"].Label != "solo" {
		t.Errorf("solo label=%q want name fallback", byName["solo"].Label)
	}
}

// TestDeriveInstancesFallback 读不出配置 / 无 instances 分节 → 返回 nil（调用方走回退）。
func TestDeriveInstancesFallback(t *testing.T) {
	dir := t.TempDir()
	if got := deriveInstances(filepath.Join(dir, "nope.json"), dir); got != nil {
		t.Errorf("missing config: want nil, got %v", got)
	}
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"listen":":7863"}`), 0o600)
	if got := deriveInstances(cfg, dir); got != nil {
		t.Errorf("single-instance config: want nil, got %v", got)
	}
}

// TestConfigMulti 多实例格式才追加 -instance；原版式单实例配置不能传（flag 不存在）。
func TestConfigMulti(t *testing.T) {
	dir := t.TempDir()
	multi := &instance{Name: "cn", Config: "config.json", root: dir}
	os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"listen":":7864","instances":{"cn":{"listen":":7864"}}}`), 0o600)
	if !multi.configMulti() {
		t.Error("multi-instance config should be multi")
	}
	args := multi.startArgs()
	if len(args) != 5 || args[3] != "-instance" || args[4] != "cn" {
		t.Errorf("startArgs=%v want -instance cn appended", args)
	}

	singleRoot := t.TempDir()
	single := &instance{Name: "cn", Config: "config.json", root: singleRoot}
	os.WriteFile(filepath.Join(singleRoot, "config.json"), []byte(`{"listen":":7864"}`), 0o600)
	if single.configMulti() {
		t.Error("single-instance config should not be multi")
	}
	sargs := single.startArgs()
	if len(sargs) != 3 {
		t.Errorf("startArgs=%v want no -instance (upstream binary lacks the flag)", sargs)
	}
}

// TestAdminNotFoundFriendlyMessage 原版二进制无 /admin/*：404 要翻成人话。
func TestAdminNotFoundFriendlyMessage(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	dir := t.TempDir()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"listen":"127.0.0.1:`+port+`"}`), 0o600)
	in := &instance{Name: "cn", Label: "CN", Config: "config.json", root: dir}
	_, err := in.admin("checkin", nil)
	if err == nil {
		t.Fatal("want error for 404")
	}
	if !strings.Contains(err.Error(), "原版") || !strings.Contains(err.Error(), "/admin") {
		t.Errorf("error %q should explain the upstream binary limitation", err)
	}
}
