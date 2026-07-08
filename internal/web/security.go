package web

import (
	"crypto/subtle"
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
	// maxRequestBodyBytes 限制请求体大小，防止内存耗尽 DoS。
	maxRequestBodyBytes = 1 << 20 // 1MB
)

// withSecurity 包装 mux，统一施加 Host 校验（防 DNS rebinding）、Origin 校验（防 CSRF）、
// 令牌校验（非本地监听时的访问鉴权）与请求体大小限制。默认本地监听下前端零改动即可通过。
func (s *Server) withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			s.writeError(w, http.StatusForbidden, "Host 头不被允许")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin, r.Host) {
			s.writeError(w, http.StatusForbidden, "跨域请求被拒绝")
			return
		}
		if s.token != "" && !s.tokenValid(r) {
			s.writeError(w, http.StatusUnauthorized, "缺少或无效的访问令牌")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
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

// originAllowed 校验 Origin：其 host:port 与请求 Host 同源即通过，否则须命中 Host 白名单。
func (s *Server) originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	return s.hostAllowed(u.Host)
}

// tokenValid 从 Authorization: Bearer 头或 token 查询参数校验令牌（常量时间比较）。
func (s *Server) tokenValid(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if constantTimeEqual(strings.TrimPrefix(h, "Bearer "), s.token) {
			return true
		}
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return constantTimeEqual(q, s.token)
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
