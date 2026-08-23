package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Shell 是桌面壳的本地 HTTP 服务：WebView 的唯一入口。
// 职责：
//   - 提供内嵌 SPA（与 Web 管理台同一份构建产物）
//   - /desktop/api/*：实例注册表、自启开关、应用信息等壳层能力
//   - /api/remote/{id}/*：反代远程 Docker 实例
//   - /api/*：反代本地引擎。WebView 的源是壳服务端口，引擎在另一个回环端口上，
//     界面对本机实例使用同源相对路径，必须由本层转发；否则请求会落入 SPA 回落
//     路由返回 index.html，界面将永远停在“正在连接”。
//
// 仅监听回环随机端口，无鉴权（与引擎的回环语义一致）。
type Shell struct {
	reg        *Registry
	engineBase string // 如 http://127.0.0.1:54321
	appDir     string
	version    string
	autostart  Autostart

	baseURL string
	srv     *http.Server
}

// NewShell 构造壳服务；engineBase 为本地引擎地址，appDir 用于界面展示
func NewShell(reg *Registry, engineBase, appDir, version string, a Autostart) *Shell {
	return &Shell{reg: reg, engineBase: strings.TrimRight(engineBase, "/"), appDir: appDir, version: version, autostart: a}
}

// Start 绑定 127.0.0.1 随机端口并在后台开始服务；返回可导航给 WebView 的根地址
func (s *Shell) Start(ctx context.Context) (string, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("绑定壳监听端口失败: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", webUI())
	// 本地引擎反代：/api/remote/{id} 注册了更具体的模式，ServeMux 最长匹配优先，
	// 远程实例反代不受本条影响。
	engineURL, err := url.Parse(s.engineBase)
	if err != nil {
		return "", fmt.Errorf("解析引擎地址失败: %w", err)
	}
	mux.Handle("/api/", localEngineProxy(engineURL))
	mux.HandleFunc("GET /desktop/api/info", s.handleInfo)
	mux.HandleFunc("GET /desktop/api/instances", s.handleList)
	mux.HandleFunc("POST /desktop/api/instances", s.handleAdd)
	mux.HandleFunc("PUT /desktop/api/instances/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE /desktop/api/instances/{id}", s.handleDelete)
	mux.HandleFunc("POST /desktop/api/instances/{id}/test", s.handleTest)
	mux.HandleFunc("POST /desktop/api/select", s.handleSelect)
	mux.HandleFunc("GET /desktop/api/autostart", s.handleAutostartGet)
	mux.HandleFunc("PUT /desktop/api/autostart", s.handleAutostartSet)

	// 远程实例反代：按路径段动态分发到对应实例的 proxy
	mux.Handle(RemotePrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.SplitN(strings.TrimPrefix(r.URL.Path, RemotePrefix), "/", 2)[0]
		if id == "" {
			writeErr(w, http.StatusNotFound, "缺少实例 ID")
			return
		}
		rp, err := newInstanceProxy(s.reg, id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		rp.ServeHTTP(w, r)
	}))

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		// 不设 ReadTimeout/WriteTimeout：SSE 与媒体流为长连接
	}
	go func() {
		// ctx 取消时经 Shutdown 收尾
		_ = s.srv.Serve(ln)
	}()
	s.baseURL = "http://" + ln.Addr().String()
	return s.baseURL, nil
}

// Stop 优雅关闭壳服务
func (s *Shell) Stop() {
	if s.srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}
}

// localEngineProxy 返回把 /api/* 原样转发给本地引擎的反向代理。
//   - 路径与查询串（含引擎自身的 ?token= 鉴权语义）不动；
//   - 剥离本机页面附带的 Origin/Referer/Cookie：引擎按严格同源校验 Origin，
//     而壳端口 ≠ 引擎端口，不剥离会让所有 POST 被 403；
//   - FlushInterval=-1 逐字节冲刷，保证 SSE 事件流不经过缓冲。
func localEngineProxy(target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     newProxyTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Del("Origin")
			req.Header.Del("Referer")
			req.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, "无法连接到本地下载引擎: "+sanitizeErr(err))
		},
	}
	return rp
}

const (
	readHeaderTimeout = 10 * time.Second
	shutdownWait      = 3 * time.Second

	// maxRequestBodyBytes 限制壳层 API 的请求体大小
	maxRequestBodyBytes = 64 << 10
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return body, false
	}
	return body, true
}
