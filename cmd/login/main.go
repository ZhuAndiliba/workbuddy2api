// login.go — WorkBuddy OAuth 登录（设备授权流程，CN / 国际站双区）。
//
// 三个子命令，由 login.sh 顺序驱动：
//
//	login url   [cn|global] → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	                          state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login poll  [cn|global] → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	                          成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	                          stdout 打印完整 token+account JSON
//
// 区域决定上游 host 与 Origin（CN: copilot.tencent.com / www.codebuddy.cn；
// 国际站: www.workbuddy.ai），两者是独立部署，凭证不可混用。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"time"
)

// 上游常量：CN 与国际站各自一套 host / Origin。
const (
	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	baseCN     = "https://copilot.tencent.com"
	originCN   = "https://www.codebuddy.cn"
	baseGlobal = "https://www.workbuddy.ai"
	originGlob = "https://www.workbuddy.ai"

	endpointAuthState = "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct = "/v2/plugin/login/account?state="
	endpointAuthToken = "/v2/plugin/auth/token?state="

	stateFile = "/tmp/wb2api-login-state.json"
)

// regionEndpoints 单个区域的上游入口。
type regionEndpoints struct {
	base   string // 上游 host（/v2/plugin/* 挂在这里）
	origin string // Origin/Referer（上游按此校验来源区域）
}

// endpointsFor 返回区域对应的入口；未识别区域回落 CN。
func endpointsFor(region string) regionEndpoints {
	if region == "global" {
		return regionEndpoints{base: baseGlobal, origin: originGlob}
	}
	return regionEndpoints{base: baseCN, origin: originCN}
}

// commonHeaders 通用请求头（Origin/Referer 随区域变化）。
func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error。
// ep 提供该区域的通用头（Origin/Referer）；headers 非 nil 时在其之上追加专属头（如 Bearer）。
func doJSON(client *http.Client, method, fullURL string, ep regionEndpoints, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(ep.origin)(req)
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

// loginState 落盘的登录中间态；region 决定 poll 阶段打哪个区（两区 host 不同）。
type loginState struct {
	State  string `json:"state"`
	Region string `json:"region"`
}

// parseRegion 取命令行区域参数（默认 cn）；未识别值直接报错，避免静默打错区。
func parseRegion(args []string) string {
	r := "cn"
	if len(args) > 0 && args[0] != "" {
		r = args[0]
	}
	switch r {
	case "cn", "global":
		return r
	}
	fatal("unknown region %q (want cn|global)", r)
	return "" // unreachable
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll> [cn|global]")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch os.Args[1] {
	case "url":
		region := parseRegion(os.Args[2:])
		ep := endpointsFor(region)
		// handleStartLogin (oauth.go:68-87)
		data, _, err := doJSON(client, http.MethodPost, ep.base+endpointAuthState, ep, nil, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
			fatal("auth state: missing state or authUrl")
		}
		raw, _ := json.Marshal(loginState{State: st.State, Region: region})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		// 区域以 state 文件为准（url 阶段已固定），命令行参数仅作显式覆盖。
		region := ls.Region
		if len(os.Args) > 2 {
			region = parseRegion(os.Args[2:])
		}
		ep := endpointsFor(region)
		// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
		// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
		tokRaw, status, errTok := doJSON(client, http.MethodGet, ep.base+endpointAuthToken+ls.State, ep, nil, nil)
		if errTok != nil {
			if status == 0 || status >= 500 {
				fatal("token endpoint error: %v", errTok)
			}
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		// login/account 拿 uid/nickname（带 Bearer）
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		acctHeaders := func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, ep.base+endpointLoginAcct+ls.State, ep, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		// 上游未回 domain 时按区域兜底写入，使网关能据此判定 region（关键：决定 host/Origin）。
		domain := tok.Domain
		if domain == "" {
			if region == "global" {
				domain = "www.workbuddy.ai"
			} else {
				domain = "copilot.tencent.com"
			}
		}
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        domain,
			"region":        region,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}
