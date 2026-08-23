package desktop

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// RemotePrefix 是远程实例反代的路由前缀，完整路径形如 /api/remote/{id}/api/state
const RemotePrefix = "/api/remote/"

// newInstanceProxy 构造指向指定实例的反向代理：
//   - 剥离本地 Cookie / Origin / Referer / 本地 token 查询参数（远端有独立鉴权体系）
//   - 注入 Authorization: Bearer 与 token 查询参数：后者覆盖 EventSource、<img> 等
//     无法自定义请求头的场景（与网页端 ?token= 语义一致）
//   - FlushInterval=-1 逐字节冲刷，保证 SSE 事件流不经过缓冲
func newInstanceProxy(reg *Registry, id string) (*httputil.ReverseProxy, error) {
	in, ok := reg.Get(id)
	if !ok {
		return nil, fmt.Errorf("实例不存在: %s", id)
	}
	target, err := url.Parse(in.URL)
	if err != nil {
		return nil, fmt.Errorf("实例地址解析失败: %w", err)
	}

	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // SSE 必需：立即冲刷
		Transport:     newProxyTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// 改写路径：去掉 /api/remote/{id} 前缀，保留其余部分
			p := strings.TrimPrefix(req.URL.Path, RemotePrefix+id)
			if p == "" || !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			req.URL.Path = p
			req.URL.RawPath = ""

			// 远端按自身规则鉴权；本地的查询参数令牌语义无关且可能引发误判，先剥掉
			q := req.URL.Query()
			q.Del("token")
			if in.Token != "" {
				q.Set("token", in.Token)
			}
			req.URL.RawQuery = q.Encode()

			req.Header.Del("Origin")
			req.Header.Del("Referer")
			req.Header.Del("Cookie")
			if in.Token != "" {
				req.Header.Set("Authorization", "Bearer "+in.Token)
			} else {
				req.Header.Del("Authorization")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"无法连接到远程实例: ` + sanitizeErr(err) + `"}`))
		},
	}
	return rp, nil
}

func sanitizeErr(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, `"`, "'")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// newProxyTransport 提供带响应头超时的传输层：SSE 长连接不受读超时影响，
// 但连不上/无响应的远端能在有限时间内报错而不是挂死页面。
func newProxyTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 20 * time.Second
	return t
}
