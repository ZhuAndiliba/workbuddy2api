// console 常驻控制台：网页查看 CN / 国际站实例的运行状态与账号池，并点击启停 / 运维。
//
// 为什么必须是独立进程：网关的面板 /ui 由实例自己提供，实例一停页面就没了，
// 无法用它来"启动自己"。所以启停控制必须由一个不随实例生灭的进程提供。
//
// 它做三件事：
//  1. 生命周期：启动 / 停止 / 重启每个实例（直接操作本机进程）；
//  2. 账号池总览：聚合两个实例的账号（积分/状态/成功率/在途），一屏看全；
//  3. 运维动作：立即签到、立即保活刷新 token、手动解冻冷却（转发到实例的 /admin/*）。
//
// 安全边界（进程控制比看状态敏感得多）：
//   - 默认只监听回环地址 127.0.0.1，局域网/公网访问不到；
//   - 若 -listen 绑到非回环地址，则必须提供 -token，否则拒绝启动（不做"裸奔控制面"）；
//   - 设了 -token 后，所有 /api/* 请求必须带 X-Console-Token 头或 ?token= 参数。
//
// 与 run.sh 的互操作：沿用同一套 data/run/<name>.pid 与 data/logs/<name>.log 约定，
// 因此 run.sh 启动的实例控制台能识别，反之亦然。
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed console.html
var consoleHTML []byte

const (
	defaultListen = "127.0.0.1:7860"
	stopWait      = 5 * time.Second
	probeTimeout  = 3 * time.Second
	logTailBytes  = 256 << 10 // 日志尾部最多读 256 KiB
	modelsTTL     = time.Minute
	adminTimeout  = 3 * time.Minute // 签到/保活要逐个账号打上游
)

// instance 一个受管的本机实例（由 ./wb2api 子进程承载）。
type instance struct {
	Name   string `json:"name"`   // cn / global（用于 pid/日志文件名与 API 标识）
	Label  string `json:"label"`  // 展示名
	Config string `json:"config"` // 配置文件（相对 root）

	root string // 项目根目录
}

func (in *instance) pidFile() string { return filepath.Join(in.root, "data", "run", in.Name+".pid") }
func (in *instance) logFile() string { return filepath.Join(in.root, "data", "logs", in.Name+".log") }

// instanceConfig 只取控制台需要的字段。
type instanceConfig struct {
	Listen string `json:"listen"`
	APIKey string `json:"api_key"`
}

func (in *instance) readConfig() (instanceConfig, error) {
	var c instanceConfig
	raw, err := os.ReadFile(filepath.Join(in.root, in.Config))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, nil
}

func (in *instance) port() string {
	c, err := in.readConfig()
	if err != nil {
		return ""
	}
	return portOf(c.Listen)
}

func (in *instance) baseURL() string {
	port := in.port()
	if port == "" {
		return ""
	}
	return "http://127.0.0.1:" + port
}

// portOf 抽出 listen 里的端口号；无法解析时返回空串。
func portOf(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return strings.TrimSpace(listen[i+1:])
	}
	return listen
}

func (in *instance) readPID() int {
	raw, err := os.ReadFile(in.pidFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// alive 报告 pid 对应进程是否仍存活（signal 0 探测，不发信号）。
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// running 返回实例是否在跑及其 pid。
// 判活有两条通道，任一成立即算在跑：
//  1. pid 文件里的进程存活——本机直跑（./run.sh / 控制台自己拉起）时成立；
//  2. /healthz 可达——容器内运行时 pid 文件在容器里、控制台看不到，
//     但能通过回环端口探到（容器内同网络命名空间）。
//
// 只认 pid 会在容器部署下误报"已停止"（控制台看不到容器内的进程），
// 进而让启动/停止按钮状态错乱，故必须叠加可达性判断。
func (in *instance) running() (bool, int) {
	pid := in.readPID()
	if alive(pid) {
		return true, pid
	}
	if in.reachable() {
		return true, 0 // 在跑但不是本进程能管的 pid（如容器内）
	}
	return false, 0
}

// reachable 报告实例的 /healthz 是否可达（含 503：进程活着但无可用账号）。
func (in *instance) reachable() bool {
	base := in.baseURL()
	if base == "" {
		return false
	}
	resp, err := (&http.Client{Timeout: time.Second}).Get(base + "/healthz")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}

// portBusy 报告端口是否已被占用（用于启动前的冲突检查）。
func portBusy(port string) bool {
	if port == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// start 启动实例（后台子进程），日志追加到 data/logs/<name>.log。
// 子进程与本控制台解耦：控制台退出不会带走实例。
func (in *instance) start() error {
	if ok, pid := in.running(); ok {
		if pid == 0 {
			return fmt.Errorf("已在运行（进程不在本控制台可管理的范围内，如运行于容器中）")
		}
		return fmt.Errorf("已在运行 (pid %d)", pid)
	}
	port := in.port()
	if portBusy(port) {
		return fmt.Errorf("端口 %s 已被占用：可能由 run.sh 启动，请先停掉再试", port)
	}
	bin := filepath.Join(in.root, "wb2api")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("找不到 %s（先编译：./run.sh build）", bin)
	}
	if _, err := in.readConfig(); err != nil {
		return fmt.Errorf("读配置 %s 失败: %w", in.Config, err)
	}
	if err := os.MkdirAll(filepath.Dir(in.logFile()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(in.pidFile()), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(in.logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()

	cmd := exec.Command(bin, "-config", in.Config)
	cmd.Dir = in.root
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // 脱离本进程管理，之后靠 pid 文件追踪
	if err := os.WriteFile(in.pidFile(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("已启动 (pid %d) 但写 pid 文件失败: %w", pid, err)
	}
	return nil
}

// stop 让实例优雅退出（SIGTERM → 等待 → SIGKILL）。
func (in *instance) stop() error {
	ok, pid := in.running()
	if !ok {
		_ = os.Remove(in.pidFile())
		return fmt.Errorf("未在运行")
	}
	// pid==0 表示实例可达但进程不在本控制台的视野内（典型：跑在容器里）。
	// 不能瞎发信号，给出可操作的提示而不是含糊失败。
	if pid == 0 {
		return fmt.Errorf("检测到实例在运行，但进程不在本控制台可管理的范围内（如运行于容器中）；" +
			"请在对应环境里停止（例如 docker compose stop）")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = p.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			_ = os.Remove(in.pidFile())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Signal(syscall.SIGKILL)
	_ = os.Remove(in.pidFile())
	return fmt.Errorf("未在 %s 内退出，已强制结束 (pid %d)", stopWait, pid)
}

// account 实例 /status 里的单个账号（字段与网关 pool.Status 对齐）。
type account struct {
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	Region        string `json:"region"`
	Credits       int64  `json:"credits"`
	Cooling       bool   `json:"cooling"`
	CoolKind      string `json:"cool_kind"`
	CoolRemaining int64  `json:"cool_remaining_sec"`
	Disabled      bool   `json:"disabled"`
	Reason        string `json:"reason"`
	SuccessCount  int64  `json:"success_count"`
	ErrTotal      int64  `json:"err_total"`
	InFlight      int    `json:"in_flight"`
	BreakerFails  int    `json:"breaker_fails"`
	LastSuccess   string `json:"last_success"`
}

// statusResp 实例 /status 的响应（只取控制台要用的字段）。
type statusResp struct {
	Region         string    `json:"region"`
	Total          int       `json:"total"`
	Healthy        int       `json:"healthy"`
	Cooling        int       `json:"cooling"`
	Disabled       int       `json:"disabled"`
	InFlightFull   int       `json:"in_flight_full"`
	StickySessions int       `json:"sticky_sessions"`
	Accounts       []account `json:"accounts"`
}

// probe 拉取实例的 healthz 与 /status（带该实例自己的 api_key）。
func (in *instance) probe() (health int, st *statusResp) {
	base := in.baseURL()
	if base == "" {
		return 0, nil
	}
	c, err := in.readConfig()
	if err != nil {
		return 0, nil
	}
	client := &http.Client{Timeout: probeTimeout}

	if resp, err := client.Get(base + "/healthz"); err == nil {
		health = resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	req, err := http.NewRequest(http.MethodGet, base+"/status", nil)
	if err != nil {
		return health, nil
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return health, nil
	}
	defer resp.Body.Close()
	var s statusResp
	if json.NewDecoder(resp.Body).Decode(&s) == nil {
		st = &s
	}
	return health, st
}

// fetchModels 取实例的模型列表（/v1/models）。
func (in *instance) fetchModels() []string {
	base := in.baseURL()
	if base == "" {
		return nil
	}
	c, err := in.readConfig()
	if err != nil {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// admin 转发一个运维动作到实例的 /admin/*（复用实例自己的 api_key）。
func (in *instance) admin(action string, body []byte) (string, error) {
	base := in.baseURL()
	if base == "" {
		return "", fmt.Errorf("读不到监听端口")
	}
	c, err := in.readConfig()
	if err != nil {
		return "", fmt.Errorf("读配置失败: %w", err)
	}
	if !portBusy(in.port()) {
		return "", fmt.Errorf("实例未运行")
	}
	if body == nil {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, base+"/admin/"+action, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := (&http.Client{Timeout: adminTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if !out.OK {
		return out.Message, fmt.Errorf("%s", out.Message)
	}
	return out.Message, nil
}

// instanceView 单个实例对前端暴露的完整状态。
type instanceView struct {
	Name    string      `json:"name"`
	Label   string      `json:"label"`
	Port    string      `json:"port"`
	Running bool        `json:"running"`
	PID     int         `json:"pid"`
	Health  int         `json:"health"`
	Status  *statusResp `json:"status,omitempty"`
	Models  []string    `json:"models,omitempty"`
	Panel   string      `json:"panel"`
	Note    string      `json:"note"`
}

// Server 控制台。
type Server struct {
	root      string
	instances []*instance
	token     string
	mu        sync.Mutex // 串行化生命周期动作，避免连点造成并发启停

	modelsMu    sync.Mutex
	modelsCache map[string]modelsEntry
}

type modelsEntry struct {
	ids []string
	at  time.Time
}

func (s *Server) findInstance(name string) *instance {
	for _, in := range s.instances {
		if in.Name == name {
			return in
		}
	}
	return nil
}

// models 带 60s 缓存地取某实例模型列表（网关侧本身缓存 1h，这里只是别让 3s 刷新打太密）。
func (s *Server) models(in *instance) []string {
	s.modelsMu.Lock()
	if e, ok := s.modelsCache[in.Name]; ok && time.Since(e.at) < modelsTTL {
		s.modelsMu.Unlock()
		return e.ids
	}
	s.modelsMu.Unlock()

	ids := in.fetchModels()
	s.modelsMu.Lock()
	if s.modelsCache == nil {
		s.modelsCache = map[string]modelsEntry{}
	}
	// 拉取失败（nil）不写缓存，下次刷新立即重试。
	if ids != nil {
		s.modelsCache[in.Name] = modelsEntry{ids: ids, at: time.Now()}
	}
	s.modelsMu.Unlock()
	return ids
}

// totals 跨实例汇总（前端顶部概览用）。
type totals struct {
	Instances int   `json:"instances"`
	Running   int   `json:"running"`
	Accounts  int   `json:"accounts"`
	Healthy   int   `json:"healthy"`
	Cooling   int   `json:"cooling"`
	Disabled  int   `json:"disabled"`
	Credits   int64 `json:"credits"`
}

func (s *Server) state() map[string]any {
	views := make([]instanceView, 0, len(s.instances))
	var tt totals
	for _, in := range s.instances {
		running, pid := in.running()
		health, st := in.probe()
		v := instanceView{
			Name:    in.Name,
			Label:   in.Label,
			Port:    in.port(),
			Running: running,
			PID:     pid,
			Health:  health,
			Status:  st,
		}
		if b := in.baseURL(); b != "" {
			v.Panel = b + "/ui"
		}
		if _, err := in.readConfig(); err != nil {
			v.Note = "读配置失败: " + err.Error()
		}
		if running {
			v.Models = s.models(in)
		}
		tt.Instances++
		if running {
			tt.Running++
		}
		if st != nil {
			tt.Accounts += st.Total
			tt.Healthy += st.Healthy
			tt.Cooling += st.Cooling
			tt.Disabled += st.Disabled
			for _, a := range st.Accounts {
				tt.Credits += a.Credits
			}
		}
		views = append(views, v)
	}
	return map[string]any{
		"ts":        time.Now().Unix(),
		"instances": views,
		"totals":    tt,
	}
}

// --- HTTP ---

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	if r.Header.Get("X-Console-Token") == s.token {
		return true
	}
	return r.URL.Query().Get("token") == s.token
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "token 无效"})
		return
	}
	writeJSON(w, http.StatusOK, s.state())
}

// handleAction 执行生命周期或运维动作。生命周期动作串行化，避免连点并发启停。
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "token 无效"})
		return
	}
	var req struct {
		Target string `json:"target"`
		Action string `json:"action"`
		UID    string `json:"uid"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求体解析失败"})
		return
	}

	in := s.findInstance(req.Target)
	if in == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "未知实例 " + req.Target})
		return
	}

	// 运维动作转发给实例。不占生命周期锁：签到可能跑几十秒，不该阻塞启停按钮。
	switch req.Action {
	case "checkin", "keepalive", "revive":
		var body []byte
		if req.Action == "revive" && req.UID != "" {
			body, _ = json.Marshal(map[string]string{"uid": req.UID})
		}
		msg, err := in.admin(req.Action, body)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": in.Label + " " + msg})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	done := ""
	switch req.Action {
	case "start":
		err = in.start()
		done = in.Label + " 已启动"
	case "stop":
		err = in.stop()
		done = in.Label + " 已停止"
	case "restart":
		if e := in.stop(); e != nil && !strings.Contains(e.Error(), "未在运行") {
			err = e
		} else if e := in.start(); e != nil {
			err = e
		}
		done = in.Label + " 已重启"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "message": "action 只能是 start/stop/restart/checkin/keepalive/revive",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	// 生命周期动作后清一次模型缓存，避免停/起后展示过期列表。
	s.modelsMu.Lock()
	delete(s.modelsCache, in.Name)
	s.modelsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": done})
}

// handleLogs 返回指定实例的日志尾部。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "token 无效"})
		return
	}
	in := s.findInstance(r.URL.Query().Get("target"))
	if in == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "未知实例"})
		return
	}
	lines := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 && v <= 5000 {
		lines = v
	}
	text, err := tailFile(in.logFile(), lines)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": "（日志不可读：" + err.Error() + "）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": text})
}

// tailFile 取文件末尾 lines 行；最多读尾部 logTailBytes，避免大日志把内存拉满。
func tailFile(path string, lines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if st.Size() > logTailBytes {
		start = st.Size() - logTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	br := bufio.NewReaderSize(f, 64*1024)
	if start > 0 {
		// 从文件中段开始，首行几乎必然是半截的（正好落在行中间），丢掉它，
		// 否则展示出来的第一行会是乱码般的残句。
		if _, err := br.ReadString('\n'); err != nil && err != io.EOF {
			return "", err
		}
	}
	var all []string
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	_, _ = w.Write(consoleHTML)
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/action", s.handleAction)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	return mux
}

// isLoopbackHost 判断监听地址是否只对本机可见。
func isLoopbackHost(listen string) bool {
	host := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		host = listen[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "::1", "localhost", "127.0.0.2":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

func main() {
	listen := flag.String("listen", defaultListen, "监听地址（默认仅本机可访问）")
	token := flag.String("token", "", "控制令牌；绑非回环地址时必填")
	root := flag.String("root", ".", "项目根目录（含 config.*.json / wb2api）")
	flag.Parse()

	if !isLoopbackHost(*listen) && *token == "" {
		log.Fatalf("拒绝启动：-listen=%s 对非本机可见，但未设置 -token。进程控制面不允许裸奔。", *listen)
	}

	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("root 解析失败: %v", err)
	}

	s := &Server{
		root: abs,
		instances: []*instance{
			{Name: "cn", Label: "CN", Config: "config.cn.json", root: abs},
			{Name: "global", Label: "国际站", Config: "config.global.json", root: abs},
		},
		token:       *token,
		modelsCache: map[string]modelsEntry{},
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("控制台 listening on http://%s/ （受管实例：cn :%s / global :%s）",
		*listen, s.instances[0].port(), s.instances[1].port())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}
