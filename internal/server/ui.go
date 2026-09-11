// ui.go 内嵌的运维面板：浏览器里查看账号池 / 模型，并直接做聊天测试。
//
// 设计取舍：
//   - 用 go:embed 把单文件 HTML 编进二进制，不依赖外部静态文件（无需额外挂载任何目录）；
//   - 面板本身**不鉴权**（只是个空壳 HTML，不含任何密钥）；它展示的数据由前端带
//     Authorization 头调 /status、/v1/models 获取，因此仍受 api_key 保护——
//     与他人拿到 /healthz 一样，拿到页面本身不泄露任何信息；
//   - API Key 由使用者填在浏览器里（localStorage），不经过服务端存储。
package server

import (
	_ "embed"
	"net/http"
)

//go:embed ui.html
var uiHTML []byte

// ui 返回面板页面。恒无鉴权：页面是静态的，数据接口各自鉴权。
func (h *Handler) ui(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板是纯本机运维工具，禁止被第三方站点内嵌，避免被钓鱼页面套壳。
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	_, _ = w.Write(uiHTML)
}

// root 把根路径重定向到面板，省得记 /ui。
func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/ui", http.StatusFound)
}
