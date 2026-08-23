package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const (
	// webTokenEnv 提供访问令牌；监听非本地地址时必需，本地监听可选。
	webTokenEnv = "TG_DOWN_WEB_TOKEN" // #nosec G101 -- 环境变量名，非硬编码凭据
	// allowedHostsEnv 是额外放行的 Host 白名单（逗号分隔），用于反向代理等场景。
	allowedHostsEnv = "TG_DOWN_WEB_ALLOWED_HOSTS"
	// trustProxyEnv 显式开启对 X-Forwarded-Proto 的信任。只有应用无法被绕过代理直连时才应启用。
	trustProxyEnv = "TG_DOWN_WEB_TRUST_PROXY"
	// maxRequestBodyBytes 限制请求体大小，防止内存耗尽 DoS。
	maxRequestBodyBytes = 1 << 20 // 1MB
	// webTokenCookie 保存令牌引导后的会话凭据。Cookie 中存令牌摘要而非原文，
	// HttpOnly + SameSite=Strict 使媒体与 SSE 可自动鉴权，同时不把令牌暴露给前端脚本。
	webTokenCookie = "tg_down_web_auth" //nolint:gosec // Cookie 名称，不是凭据本身
	// authCapabilityCookie 不含秘密，只让新版前端识别后端支持 HttpOnly Cookie 引导，
	// 从而一次性迁移旧 localStorage 令牌。它必须可由 JavaScript 读取。
	authCapabilityCookie = "tg_down_web_cookie_auth"

	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// contentSecurityPolicy 限制页面可加载的资源来源。
//
//   - script-src 不含 'unsafe-inline'：Vite 的产物里没有一行内联脚本（旧版把 1600 行 JS
//     写在 HTML 里，那时只能开着 unsafe-inline，等于把 CSP 对 XSS 的防护基本让掉）；
//   - style-src 仍需 'unsafe-inline'：组件里用了行内 style 属性；
//   - img/media 允许 blob:，画廊的灯箱需要；
//   - connect-src 'self' 覆盖 fetch 与 SSE；
//   - frame-ancestors 'none' 与 X-Frame-Options 一起挡点击劫持。
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"media-src 'self' blob:; " +
	"connect-src 'self'; " +
	"font-src 'self' data:; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// setSecurityHeaders 施加一组安全响应头。此前一个都没有：
// 媒体端点会把 Telegram 上他人提供的文件原样吐给浏览器，没有 CSP / nosniff 的话，
// 一个伪装成图片的 HTML 文件就能在管理台的同源上下文里执行脚本。
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// withSecurity 包装 mux，统一施加安全响应头、Host 校验（防 DNS rebinding）、Origin 校验（防 CSRF）、
// 令牌校验（非本地监听时的访问鉴权）与请求体大小限制。
//
// UI 文档和哈希静态资源本身不含私有数据，可以公开读取；否则首次打开 /?token=...
// 时，index.html 引用的 JS/CSS 不会继承查询参数，页面会在脚本执行前就收到 401。
// 有效的 UI token 查询参数会先换成 HttpOnly 会话 Cookie，再跳转到不含 token 的 URL。
// 所有 /api 请求（包括媒体与 SSE）仍必须通过 Bearer、查询参数或会话 Cookie 鉴权。
func (s *Server) withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if isAPIPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		if !s.hostAllowed(r.Host) {
			s.writeError(w, http.StatusForbidden, "Host 头不被允许")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin, r) {
			s.writeError(w, http.StatusForbidden, "跨域请求被拒绝")
			return
		}
		if s.token != "" && !isAPIPath(r.URL.Path) && r.URL.Query().Get("token") == "" {
			s.setAuthCapabilityCookie(w, r)
		}
		if s.token != "" {
			if token := r.URL.Query().Get("token"); token != "" && !isAPIPath(r.URL.Path) {
				if !constantTimeEqual(token, s.token) {
					redirectWithoutToken(w, r)
					return
				}
				s.bootstrapTokenCookie(w, r)
				return
			}
			if isAPIPath(r.URL.Path) && !s.tokenValid(r) {
				s.writeError(w, http.StatusUnauthorized, "缺少或无效的访问令牌")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setAuthCapabilityCookie(w http.ResponseWriter, r *http.Request) {
	// 不设 HttpOnly 是有意为之（见 authCapabilityCookie 注释）；Secure 按实际请求协议取值，
	// 静态检查读不出动态值。
	//nolint:gosec // 无秘密内容，需前端脚本可读
	http.SetCookie(w, &http.Cookie{
		Name:     authCapabilityCookie,
		Value:    "1",
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		Secure:   s.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// bootstrapTokenCookie 把地址栏中的令牌换成会话 Cookie，并从跳转地址移除 token。
func (s *Server) bootstrapTokenCookie(w http.ResponseWriter, r *http.Request) {
	//nolint:gosec // Secure 取自 requestIsHTTPS，静态检查读不出动态值
	http.SetCookie(w, &http.Cookie{
		Name:     webTokenCookie,
		Value:    tokenCookieValue(s.token),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	redirectWithoutToken(w, r)
}

func redirectWithoutToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	q := r.URL.Query()
	q.Del("token")
	target := localRedirectPath(r.URL)
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	// target 由 localRedirectPath 规范化为恰好一个前导斜杠、且不含反斜杠的站内路径，
	// 查询串经 url.Values.Encode 转义，不存在跳到外站的取值。
	//nolint:gosec // 已规范化为站内绝对路径
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// localRedirectPath 把请求路径规范化为恰好一个前导斜杠的站内绝对路径。
// 浏览器会把反斜杠当作 URL 分隔符，因此双斜杠、反斜杠及其编码形式一律回到根路径。
func localRedirectPath(u *url.URL) string {
	path := u.Path
	if path == "" || strings.HasPrefix(path, "//") || strings.Contains(path, `\`) {
		return "/"
	}
	path = "/" + strings.TrimLeft(path, "/")
	return (&url.URL{Path: path}).EscapedPath()
}

func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !s.trustProxy {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

func envEnabled(raw string) bool {
	return raw == "1" || strings.EqualFold(strings.TrimSpace(raw), "true")
}

func tokenCookieValue(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// hostAllowed 判断请求 Host 是否可信：本地回环名始终放行；非通配绑定地址放行其自身主机；
// 通配绑定（0.0.0.0/::）且已配置令牌时交由令牌鉴权，放行 Host 校验；另可经环境变量追加白名单。
func (s *Server) hostAllowed(host string) bool {
	h := hostOnly(host)
	if isLoopbackHost(h) {
		return true
	}
	if bindHost := hostOnly(s.addr); !isWildcardHost(bindHost) && strings.EqualFold(h, bindHost) {
		return true
	}
	for _, a := range s.allowedHosts {
		if strings.EqualFold(h, a) {
			return true
		}
	}
	return isWildcardHost(hostOnly(s.addr)) && s.token != ""
}

// originAllowed 要求 Origin 与外部请求严格同源。allowedHosts 只决定请求 Host 是否可信，
// 不能把另一个已允许主机的跨站 Origin 变成可信来源。
func (s *Server) originAllowed(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
		return false
	}
	wantScheme := schemeHTTP
	if s.requestIsHTTPS(r) {
		wantScheme = schemeHTTPS
	}
	return u.Scheme == wantScheme && strings.EqualFold(u.Host, r.Host)
}

// tokenValid 从 Authorization: Bearer、token 查询参数或引导 Cookie 校验令牌（常量时间比较）。
func (s *Server) tokenValid(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if constantTimeEqual(strings.TrimPrefix(h, "Bearer "), s.token) {
			return true
		}
	}
	if q := r.URL.Query().Get("token"); q != "" {
		if constantTimeEqual(q, s.token) {
			return true
		}
	}
	if cookie, err := r.Cookie(webTokenCookie); err == nil {
		return constantTimeEqual(cookie.Value, tokenCookieValue(s.token))
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hostOnly 去掉 host:port 中的端口，返回主机名/IP。
func hostOnly(hostport string) string {
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func isLoopbackHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func isWildcardHost(h string) bool {
	return h == "" || h == "0.0.0.0" || h == "::"
}

// isLoopbackAddr 判断监听地址是否仅限本地回环。
func isLoopbackAddr(addr string) bool {
	return isLoopbackHost(hostOnly(addr))
}

// parseAllowedHosts 解析环境变量白名单（逗号分隔，去空白与空项）。
func parseAllowedHosts(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	hosts := make([]string, 0, len(parts))
	for _, p := range parts {
		if h := strings.TrimSpace(p); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}
