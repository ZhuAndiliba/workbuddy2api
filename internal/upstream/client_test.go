package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "SaaS" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}

	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

// TestFetchModelsPrefersV3Config 主路径应为 /v3/config（官方客户端同款，两区通用）。
func TestFetchModelsPrefersV3Config(t *testing.T) {
	var hitV3, hitLegacy bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			hitV3 = true
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"gpt-5.6-sol","name":"GPT-5.6-Sol","maxInputTokens":400000,"maxOutputTokens":128000,"reasoning":{"supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["gpt-5.6-sol"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			hitLegacy = true
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"cn-only","name":"CN"}],"agents":[{"name":"cli","models":["cn-only"]}]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !hitV3 {
		t.Error("/v3/config should be the primary path")
	}
	if hitLegacy {
		t.Error("legacy path must not be called when /v3/config succeeds")
	}
	if len(infos) != 1 || infos[0].ID != "gpt-5.6-sol" {
		t.Fatalf("infos=%+v", infos)
	}
	if infos[0].ContextWindow != 400000 || infos[0].MaxTokens != 128000 {
		t.Errorf("limits not mapped: %+v", infos[0])
	}
}

// TestFetchModelsFallsBackToLegacy /v3/config 失败时回落旧接口（保证 CN 老路径仍可用）。
func TestFetchModelsFallsBackToLegacy(t *testing.T) {
	var hitLegacy bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(500, `boom`), nil
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			hitLegacy = true
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !hitLegacy {
		t.Error("legacy path should be tried after /v3/config fails")
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("infos=%+v", infos)
	}
}

// TestFetchModelsBothPathsFail 两条路径都失败时返回错误（不得静默返回空列表）。
func TestFetchModelsBothPathsFail(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `boom`), nil
	})
	if _, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error when both paths fail")
	}
}

// TestFetchModelsV3FiltersByCliAgent /v3/config 也要按 cli agent 白名单过滤，
// 且 reasoning 为 null 的模型不得导致解析失败（国际站多个模型如此）。
func TestFetchModelsV3FiltersByCliAgent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v3/config") {
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"in-cli","name":"In","maxInputTokens":1000,"maxOutputTokens":100},
				{"id":"not-in-cli","name":"Out","maxInputTokens":2000,"maxOutputTokens":200},
				{"id":"null-reasoning","name":"NR","maxInputTokens":3000,"maxOutputTokens":300,"reasoning":null}
			],"agents":[{"name":"cli","models":["in-cli","null-reasoning"]}]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ids := map[string]bool{}
	for _, mi := range infos {
		ids[mi.ID] = true
	}
	if len(infos) != 2 || !ids["in-cli"] || !ids["null-reasoning"] {
		t.Fatalf("cli whitelist not applied: %+v", infos)
	}
	if ids["not-in-cli"] {
		t.Error("model outside cli agent must be excluded")
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("hard credit should return body via status, not err: %v", err)
	}
	// caller classifies via returned body
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

// TestBasesDispatchByRegion 校验按账号区域派发上游 host：
// CN 号走 CN base，国际站号（domain 为 *.workbuddy.ai / *.codebuddy.ai）走 global base。
func TestBasesDispatchByRegion(t *testing.T) {
	c := testClient(nil)
	c.ChatBaseGlobal = "https://chat.global"
	c.BillingBaseGlob = "https://billing.global"

	cn := &auth.Auth{Domain: ""}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Errorf("cn bases wrong: %s %s", c.chatBase(cn), c.billingBase(cn))
	}

	gl := &auth.Auth{Domain: "www.workbuddy.ai"}
	if c.chatBase(gl) != "https://chat.global" || c.billingBase(gl) != "https://billing.global" {
		t.Errorf("global bases wrong: %s %s", c.chatBase(gl), c.billingBase(gl))
	}

	// nil 凭证按 CN 派发，避免调用方判空。
	if c.chatBase(nil) != "https://chat.example" || c.billingBase(nil) != "https://billing.example" {
		t.Error("nil auth must fall back to CN bases")
	}

	// 非 CN 也非国际站域名（如 example.com）按 CN 处理。
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(other) != "https://chat.example" || c.billingBase(other) != "https://billing.example" {
		t.Errorf("unknown domain must fall back to CN: %s %s", c.chatBase(other), c.billingBase(other))
	}
}

// TestNewDispatchConfiguredGlobals 生产默认值必须带国际站 base，否则国际站账号会拿到空 host。
func TestNewDispatchConfiguredGlobals(t *testing.T) {
	c := New()
	if c.ChatBaseGlobal == "" || c.BillingBaseGlob == "" {
		t.Fatalf("New() must configure global bases: chat=%q billing=%q", c.ChatBaseGlobal, c.BillingBaseGlob)
	}
	gl := &auth.Auth{Domain: "www.workbuddy.ai"}
	if c.chatBase(gl) != c.ChatBaseGlobal || c.billingBase(gl) != c.BillingBaseGlob {
		t.Errorf("global dispatch: %s %s", c.chatBase(gl), c.billingBase(gl))
	}
}

// TestOriginRefererByRegion 官方 CLI 按区域携带不同 Origin/Referer，本实现与之对齐。
// 注意：这只为行为一致性，并非安全边界——实测上游不校验 Origin（见 headers.go 注释）。
func TestOriginRefererByRegion(t *testing.T) {
	if got := originRefererFor(&auth.Auth{Domain: ""}); got != originRefererCN {
		t.Errorf("cn origin=%q want %q", got, originRefererCN)
	}
	if got := originRefererFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != originRefererGlobal {
		t.Errorf("global origin=%q want %q", got, originRefererGlobal)
	}
	if got := originRefererFor(nil); got != originRefererCN {
		t.Errorf("nil origin=%q want CN", got)
	}
}

// TestEffortsCachePerRegion 同名模型在两个区域的 supportedEfforts 不同时不得互相污染。
func TestEffortsCachePerRegion(t *testing.T) {
	c := New()
	gl := &auth.Auth{Domain: "www.workbuddy.ai"}
	cn := &auth.Auth{Domain: ""}

	// 造两套上游：CN 支持 high，国际站只支持 low。
	modelsJSON := func(efforts string) string {
		return `{"code":0,"data":{"models":[{"id":"m1","reasoning":{"supportedEfforts":[` + efforts + `]}}],"agents":[{"name":"cli","models":["m1"]}]}}`
	}
	up := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		body := modelsJSON(`"high"`)
		if strings.Contains(r.Header.Get("Origin"), "workbuddy.ai") {
			body = modelsJSON(`"low"`)
		}
		return jsonResp(200, body), nil
	})}
	c.HTTP = up
	c.ChatBaseCN = "https://cn.example"
	c.ChatBaseGlobal = "https://gl.example"

	if _, err := c.FetchModels(cn); err != nil {
		t.Fatalf("cn fetch: %v", err)
	}
	if _, err := c.FetchModels(gl); err != nil {
		t.Fatalf("global fetch: %v", err)
	}

	cnEfforts := c.effortsSnapshot(auth.RegionCN)
	glEfforts := c.effortsSnapshot(auth.RegionGlobal)
	if len(cnEfforts["m1"]) != 1 || cnEfforts["m1"][0] != "high" {
		t.Errorf("cn efforts=%v want [high]", cnEfforts["m1"])
	}
	if len(glEfforts["m1"]) != 1 || glEfforts["m1"][0] != "low" {
		t.Errorf("global efforts=%v want [low] (must not be CN's)", glEfforts["m1"])
	}
	// 未拉取过的区域返回 nil（未知 → 透传不降级）。
	if got := c.effortsSnapshot("unknown-region"); got != nil {
		t.Errorf("unknown region efforts=%v want nil", got)
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 120s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

// TestClassifySoftRateMarkers 非 429 状态码携带限流文案时也必须判为 soft_rate。
// 实测国际站以 HTTP 200 + code=14003 "too many requests" 返回限流。
func TestClassifySoftRateMarkers(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":14003,"msg":"too many requests"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"The model provider is rate-limiting requests."}`, ErrSoftRate},
		{400, `rate limit exceeded`, ErrSoftRate},
		{403, `{"msg":"usage limit reached"}`, ErrSoftRate},
		{500, `too many requests`, ErrSoftRate},
		{200, `{"msg":"请求过于频繁，请稍后重试"}`, ErrSoftRate},
		{200, `{"msg":"触发限流"}`, ErrSoftRate},
		{429, ``, ErrSoftRate},
		// session_dead 优先于限流文案（12153 更具体，且短冷却救不活死号）。
		{401, `{"code":12153,"msg":"Offline user session not found"`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found, too many requests"}`, ErrSessionDead},
		// hard 优先于限流（计费耗尽更严）。
		{402, `too many requests`, ErrHardCredit},
		{200, `{"msg":"余额不足, rate limit"}`, ErrHardCredit},
		// 无关文案不受影响。
		{400, `{"code":400,"msg":"bad request"}`, ErrClient},
		{404, `not found`, ErrNotFound},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestFetchModelsMergesExtra 配置的补充模型应追加到动态列表之后，且按 ID 去重。
// 上游清单不准（国际站的 deepseek-v4.1-flash 能调但不在任何列表里），故设此补充入口。
func TestFetchModelsMergesExtra(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v3/config") {
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000,"maxOutputTokens":100}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	c.ModelsExtra = []string{"deepseek-v4.1-flash", "hy4-preview", " glm-5.2 ", "", "  "}

	out, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ids := make([]string, 0, len(out))
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	// 期望：动态列表在前，补两个新 ID；重复的 glm-5.2 与空白项被跳过。
	want := []string{"glm-5.2", "deepseek-v4.1-flash", "hy4-preview"}
	if len(ids) != len(want) {
		t.Fatalf("ids=%v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d]=%q want %q (full=%v)", i, ids[i], want[i], ids)
		}
	}
	// 补充项不带元数据：不得凭空编造参数。
	for _, m := range out {
		if m.ID == "deepseek-v4.1-flash" && (m.ContextWindow != 0 || m.MaxTokens != 0) {
			t.Errorf("extra model must carry no fabricated limits: %+v", m)
		}
	}
}

// TestFetchModelsNoExtra 未配置补充时行为不变（不得多出任何模型）。
func TestFetchModelsNoExtra(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v3/config") {
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2"}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		}
		return jsonResp(404, `{}`), nil
	})
	out, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(out) != 1 || out[0].ID != "glm-5.2" {
		t.Errorf("out=%+v want only glm-5.2", out)
	}
}

// TestMergeModelsExtraUnit 合并函数的边界：空补充、全重复、仅为空白。
func TestMergeModelsExtraUnit(t *testing.T) {
	base := []ModelInfo{{ID: "a"}, {ID: "b"}}
	cases := []struct {
		name  string
		extra []string
		want  []string
	}{
		{"nil", nil, []string{"a", "b"}},
		{"empty", []string{}, []string{"a", "b"}},
		{"all dup", []string{"a", "b"}, []string{"a", "b"}},
		{"blank only", []string{"", "   "}, []string{"a", "b"}},
		{"new", []string{"c"}, []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeModelsExtra(base, c.extra)
			ids := make([]string, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ID)
			}
			if len(ids) != len(c.want) {
				t.Fatalf("ids=%v want %v", ids, c.want)
			}
			for i := range c.want {
				if ids[i] != c.want[i] {
					t.Errorf("ids=%v want %v", ids, c.want)
				}
			}
		})
	}
}
