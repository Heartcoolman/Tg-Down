// Package web provides a local Apple-styled web management UI for Tg-Down.
// It drives a long-lived Telegram connection, exposes chat browsing, history
// downloads and live monitoring over HTTP, and streams progress via SSE.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/notify"
	"tg-down/internal/queue"
	"tg-down/internal/store"
	"tg-down/internal/tgapi"
	"tg-down/internal/timeline"
)

// distFS 是 Vite 的构建产物（web/ 目录经 `make web` 生成）。
//
// all: 前缀不可省：Vite 的产物在 assets/ 子目录里，而 go:embed 默认跳过以 _ 或 . 开头的文件，
// 且不带 all: 时不会递归进子目录中的此类文件。
//
// 仓库里提交了 static/dist/.gitkeep，因此没跑过 npm build 也能编译（否则 embed 找不到目录，
// 会直接把 lint / 依赖提交 / 发布这几个 CI 作业一起打断）。此时 uiFS 里没有 index.html，
// handleIndex 会明确告诉用户去跑 `make web`，而不是白屏。
//
//go:embed all:static/dist
var distFS embed.FS

// uiFS 是以 static/dist 为根的前端资源
var uiFS = mustSubFS(distFS, "static/dist")

func mustSubFS(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic("embed: " + err.Error()) // 构建期就该发现，不该留到运行时
	}
	return sub
}

// uiBuilt 报告前端产物是否真的在（而不是只有占位的 .gitkeep）
func uiBuilt() bool {
	_, err := fs.Stat(uiFS, "index.html")
	return err == nil
}

const (
	// DefaultAddr is the default listen address (localhost only).
	DefaultAddr = "127.0.0.1:8080"

	shutdownTimeout       = 5 * time.Second
	readHeaderTimeout     = 10 * time.Second
	readTimeout           = 30 * time.Second // 限制整个请求体读取时长，防慢速攻击；SSE 无请求体不受影响
	snapshotInterval      = time.Second
	sseBufferSize         = 32
	authChanSize          = 1
	minWebTokenLength     = 16
	phoneMaskKeepHead     = 3
	phoneMaskKeepTail     = 2
	initialReconnectDelay = 2 * time.Second
	maxReconnectDelay     = 30 * time.Second
	logoutTaskWaitTimeout = 30 * time.Second
	reconnectFactor       = 2

	// SSE 事件类型
	eventState = "state"
	eventLog   = "log"
	eventTask  = "task"
)

// State 描述 Telegram 连接/认证状态
type State string

// 连接状态枚举
const (
	StateConnecting      State = "connecting"
	StateNeedCredentials State = "need_credentials"
	StateNeedLogin       State = "need_login"
	StateWaitingCode     State = "waiting_code"
	StateWaitingPassword State = "waiting_password"
	StateReady           State = "ready"
	StateError           State = "error"
)

// Server 是 Web 管理端
type Server struct {
	client        tgapi.Client
	store         *store.Store
	queue         *queue.Manager
	logger        *logger.Logger
	addr          string
	cfg           *config.Config // 与引擎共享的配置指针；设置页热更新经它持久化
	downloadRoot  string         // 下载根目录：媒体服务端点据此做越界校验
	token         string         // 访问令牌（TG_DOWN_WEB_TOKEN）；非本地监听时必需
	allowedHosts  []string       // 额外放行的 Host 白名单
	trustProxy    bool           // 是否信任反向代理提供的 X-Forwarded-Proto
	hub           *sseHub
	baseCtx       context.Context // 下载任务的生命周期父上下文（在 Run 中设置）
	serveHTTP     func(*http.Server) error
	queueDraining atomic.Bool

	mu       sync.RWMutex
	state    State
	stateErr string
	chats    []tgapi.ChatInfo

	// 时间线（sidecar 扫描索引）及其重建互斥锁
	timelineIndex  *timeline.Index
	timelineMu     sync.Mutex
	timelineBuiltAt time.Time

	codeCh   chan string
	passCh   chan string
	credCh   chan struct{} // Web 端提交 API 凭据的信号
	credSlot chan struct{} // 限制同一认证轮次只能接收一次凭据更新
	abortCh  chan struct{} // Web 端中止当前登录（验证码/密码步骤的"返回上一步"）
	logoutCh chan struct{} // Web 端登出完成，通知 runTelegram 重新进入认证循环
}

// errAuthAborted 标记用户主动中止登录（返回上一步），区别于真实认证失败
var errAuthAborted = errors.New("登录已被用户中止")

// New 创建 Web 管理端：按配置构建任务队列管理器并接线完成通知
func New(client tgapi.Client, st *store.Store, log *logger.Logger, addr string, cfg *config.Config) *Server {
	if addr == "" {
		addr = DefaultAddr
	}
	q := queue.NewManager(client, st, log, cfg.Queue.MaxConcurrentTasks, cfg.Queue.AutoRetryCount())
	var selfSend func(context.Context, string) error
	if cfg.Notify.TelegramSelf {
		selfSend = client.SendSelfMessage
	}
	if n := notify.New(selfSend, cfg.Notify.WebhookURL, log); n != nil {
		q.SetOnTerminal(n.TaskFinished)
	}
	return &Server{
		client:       client,
		store:        st,
		queue:        q,
		logger:       log,
		addr:         addr,
		cfg:          cfg,
		downloadRoot: cfg.Download.Path,
		token:        os.Getenv(webTokenEnv),
		allowedHosts: parseAllowedHosts(os.Getenv(allowedHostsEnv)),
		trustProxy:   envEnabled(os.Getenv(trustProxyEnv)),
		hub:          newSSEHub(),
		state:        StateConnecting,
		timelineIndex: timeline.New(),
		codeCh:       make(chan string, authChanSize),
		passCh:       make(chan string, authChanSize),
		credCh:       make(chan struct{}, authChanSize),
		credSlot:     make(chan struct{}, authChanSize),
		abortCh:      make(chan struct{}, authChanSize),
		logoutCh:     make(chan struct{}, authChanSize),
	}
}

// validateAccess 校验监听地址对应的鉴权要求（非回环或代理场景必须配置令牌）
func (s *Server) validateAccess() error {
	tokenRequired := !isLoopbackAddr(s.addr) || len(s.allowedHosts) > 0 || s.trustProxy
	if tokenRequired && s.token == "" {
		return fmt.Errorf("监听非本地地址或配置代理 Host 时必须通过环境变量 %s 设置访问令牌，否则拒绝启动", webTokenEnv)
	}
	if tokenRequired && utf8.RuneCountInString(strings.TrimSpace(s.token)) < minWebTokenLength {
		return fmt.Errorf("%s 至少需要 %d 个字符；请使用随机生成的高强度令牌", webTokenEnv, minWebTokenLength)
	}
	return nil
}

// newHTTPServer 构造带路由与安全中间件的 http.Server
func (s *Server) newHTTPServer() *http.Server {
	mux := http.NewServeMux()
	s.routes(mux)
	return &http.Server{
		Addr:              s.addr,
		Handler:           s.withSecurity(mux),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		// 不设置 WriteTimeout：SSE 事件流为长连接，写超时会中断推送。
	}
}

// prepare 启动后台组件（Telegram 连接循环、状态快照、任务队列、关闭监视）。
// 返回资源释放函数与等待全部后台退出的函数；serve 返回后必须先 stop 再 wait，
// 否则关闭监视 goroutine 会因 runCtx 未取消而永久阻塞。
func (s *Server) prepare(ctx context.Context, srv *http.Server) (stop, wait func()) {
	runCtx, stop := context.WithCancel(ctx)
	s.baseCtx = runCtx
	s.logger.SetHook(s.onLog)

	s.queue.SetOnChange(s.onTaskChange)
	var background sync.WaitGroup
	startBackground := func(run func()) {
		background.Add(1)
		go func() {
			defer background.Done()
			run()
		}()
	}
	startBackground(func() { s.runTelegram(runCtx) })
	startBackground(func() { s.snapshotLoop(runCtx) })
	startBackground(func() { s.queue.Run(runCtx) })

	// 父 ctx 已取消，关闭需独立超时窗口
	startBackground(func() {
		<-runCtx.Done()
		// 运行中任务的 ctx 派生自同一个 ctx，取消已沿调用链自动传播，
		// 此处无需再显式遍历取消。
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	return stop, background.Wait
}

// finish 收尾：释放上下文资源、撤销日志钩子、等待后台退出并归一化错误
func (s *Server) finish(serveErr error, stop, wait func()) error {
	stop()
	s.logger.SetHook(nil)
	wait()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务失败: %w", serveErr)
	}
	return nil
}

// Serve 在已绑定的监听器上运行完整服务（含 Telegram 连接），阻塞直到 ctx 取消。
// 桌面壳入口：先自行 net.Listen("tcp", "127.0.0.1:0")，经 ln.Addr() 拿到实际端口后再传入。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.validateAccess(); err != nil {
		return err
	}
	srv := s.newHTTPServer()
	stop, wait := s.prepare(ctx, srv)
	s.logger.Info("Web 管理端已启动: http://%s", ln.Addr().String())
	err := srv.Serve(ln)
	return s.finish(err, stop, wait)
}

// Run 启动后台 Telegram 连接与 HTTP 服务，阻塞直到 ctx 取消
func (s *Server) Run(ctx context.Context) error {
	if err := s.validateAccess(); err != nil {
		return err
	}
	srv := s.newHTTPServer()
	stop, wait := s.prepare(ctx, srv)

	if !isLoopbackAddr(s.addr) {
		s.logger.Info("Web 端监听非本地地址 %s，已启用访问令牌鉴权", s.addr)
	}
	s.logger.Info("Web 管理端已启动: http://%s", s.addr)

	serve := s.serveHTTP
	if serve == nil {
		serve = func(server *http.Server) error {
			var lc net.ListenConfig
			ln, err := lc.Listen(ctx, "tcp", server.Addr)
			if err != nil {
				return err
			}
			defer ln.Close() //nolint:errcheck
			return server.Serve(ln)
		}
	}
	err := serve(srv)
	return s.finish(err, stop, wait)
}

// authLoop 承载 runTelegram 的循环状态：重连退避时长，以及本轮是否占用了凭据提交槽位。
// 槽位必须在每次认证结束后释放，否则 Web 端的下一次提交会被一直挡住。
type authLoop struct {
	s          *Server
	delay      time.Duration
	submission bool
}

func (l *authLoop) markSubmission() {
	l.submission = true
	l.delay = initialReconnectDelay
}

func (l *authLoop) releaseSubmission() {
	if l.submission {
		l.s.releaseCredentialSubmission()
		l.submission = false
	}
}

// awaitCredentials 在凭据缺失时阻塞等待 Web 端提交 API ID/Hash/手机号；
// 已有凭据时以非阻塞方式消费可能刚入队的信号，确保槽位会在本轮认证后释放。
// 返回 false 表示 ctx 已取消。
func (l *authLoop) awaitCredentials(ctx context.Context) bool {
	if l.s.client.HasCredentials() {
		if !l.submission {
			select {
			case <-l.s.credCh:
				l.markSubmission()
			default:
			}
		}
		return true
	}
	l.s.setState(StateNeedCredentials)
	select {
	case <-l.s.credCh:
		l.markSubmission()
		return true
	case <-ctx.Done():
		return false
	}
}

// handleAuthFailure 处理一次认证失败：errAuthAborted 表示用户返回上一步，清理会话后立即重试；
// 其余错误进入退避等待，期间重新提交凭据则立即重试。返回 false 表示 ctx 已取消。
func (l *authLoop) handleAuthFailure(ctx context.Context, err error) bool {
	if errors.Is(err, errAuthAborted) {
		l.s.clearAbortedSession()
		l.releaseSubmission()
		l.delay = initialReconnectDelay
		return true
	}
	l.s.setError(err)
	l.s.logger.Error("Telegram 连接失败，%s 后重试: %v", l.delay, err)
	l.releaseSubmission()
	select {
	case <-time.After(l.delay):
		l.delay = min(l.delay*reconnectFactor, maxReconnectDelay)
	case <-l.s.credCh:
		l.markSubmission()
	case <-ctx.Done():
		return false
	}
	return true
}

// awaitSessionEnd 就绪后阻塞至退出登录或 ctx 取消。返回 false 表示 ctx 已取消。
func (l *authLoop) awaitSessionEnd(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-l.s.logoutCh:
		// 会话已在 handleAuthLogout 中销毁，清空聊天缓存后重新进入认证循环
		l.s.mu.Lock()
		l.s.chats = nil
		l.s.mu.Unlock()
		l.delay = initialReconnectDelay
		return true
	}
}

// clearAbortedSession 清理未完成的登录会话与手机号，让界面回到凭据输入页
func (s *Server) clearAbortedSession() {
	if err := s.client.ClearSession(); err != nil {
		s.logger.Warn("清理登录会话失败: %v", err)
	}
	if err := s.client.ClearPhone(); err != nil {
		s.logger.Warn("清除手机号失败: %v", err)
	}
}

// resumeQueue 认证成功后恢复处于排空状态的任务队列。返回 false 表示恢复失败，需重新认证。
func (s *Server) resumeQueue() bool {
	if !s.queueDraining.Load() {
		return true
	}
	if err := s.queue.EndDrain(); err != nil {
		s.setError(err)
		s.logger.Error("恢复任务队列失败: %v", err)
		return false
	}
	s.queueDraining.Store(false)
	return true
}

// runTelegram 连接并认证 Telegram；TDLib 在授权后自行维持/重连，
// 故仅在初始连接失败时退避重试。验证码/密码经 webCode/webPassword 注入。
func (s *Server) runTelegram(ctx context.Context) {
	l := &authLoop{s: s, delay: initialReconnectDelay}
	defer l.releaseSubmission()
	for {
		if !l.awaitCredentials(ctx) {
			s.client.Close()
			return
		}

		s.drainAuthSignals()
		s.setState(StateConnecting)
		err := s.client.AuthenticateWith(ctx, s.webCode, s.webPassword)
		if ctx.Err() != nil {
			l.releaseSubmission()
			s.client.Close()
			return
		}
		if err != nil {
			if !l.handleAuthFailure(ctx, err) {
				s.client.Close()
				return
			}
			continue
		}
		l.releaseSubmission()
		if !s.resumeQueue() {
			continue
		}

		s.setState(StateReady)
		s.logger.Info("Telegram 已连接，Web 端就绪")
		s.refreshChats(ctx)

		if !l.awaitSessionEnd(ctx) {
			s.client.Close()
			return
		}
	}
}

func (s *Server) reserveCredentialSubmission() bool {
	select {
	case s.credSlot <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseCredentialSubmission() {
	select {
	case <-s.credSlot:
	default:
	}
}

// drainAuthSignals 丢弃上一轮认证遗留的过期信号，避免误触发本轮的验证码提交或中止
func (s *Server) drainAuthSignals() {
	for {
		select {
		case <-s.codeCh:
		case <-s.passCh:
		case <-s.abortCh:
		default:
			return
		}
	}
}

// webCode 等待 Web 端提交验证码；用户可经 abortCh 中止本次登录
func (s *Server) webCode(ctx context.Context) (string, error) {
	s.setState(StateWaitingCode)
	select {
	case code := <-s.codeCh:
		return code, nil
	case <-s.abortCh:
		return "", errAuthAborted
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// webPassword 等待 Web 端提交两步验证密码；用户可经 abortCh 中止本次登录
func (s *Server) webPassword(ctx context.Context) (string, error) {
	s.setState(StateWaitingPassword)
	select {
	case pw := <-s.passCh:
		return pw, nil
	case <-s.abortCh:
		return "", errAuthAborted
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Server) setState(state State) {
	s.mu.Lock()
	s.state = state
	s.stateErr = ""
	s.mu.Unlock()
}

func (s *Server) setError(err error) {
	s.mu.Lock()
	s.state = StateError
	s.stateErr = err.Error()
	s.mu.Unlock()
}

func (s *Server) currentState() (state State, errMsg string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state, s.stateErr
}

func (s *Server) beginLogout() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateReady {
		return false
	}
	s.state = StateConnecting
	s.stateErr = ""
	return true
}

func (s *Server) refreshChats(ctx context.Context) {
	chats, err := s.client.GetChats(ctx)
	if err != nil {
		s.logger.Error("获取聊天列表失败: %v", err)
		return
	}
	s.mu.Lock()
	s.chats = chats
	s.mu.Unlock()
	s.logger.Info("已加载 %d 个聊天", len(chats))
}

// onTaskChange 是 queue.Manager 的任务生命周期变化回调，序列化后经 SSE 广播
func (s *Server) onTaskChange(dto *queue.TaskDTO) {
	if data, err := json.Marshal(dto); err == nil {
		s.hub.broadcast(sseMessage{Event: eventTask, Data: string(data)})
	}
}

// snapshotLoop 定时向 SSE 广播状态快照
func (s *Server) snapshotLoop(ctx context.Context) {
	ticker := time.NewTicker(snapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if data, err := json.Marshal(s.snapshot()); err == nil {
				s.hub.broadcast(sseMessage{Event: eventState, Data: string(data)})
			}
		case <-ctx.Done():
			return
		}
	}
}

// onLog 将日志广播给所有 SSE 订阅者（必须非阻塞）
func (s *Server) onLog(level, msg string) {
	entry := logEntry{
		Time:  time.Now().Format("15:04:05"),
		Level: level,
		Msg:   msg,
	}
	if data, err := json.Marshal(entry); err == nil {
		s.hub.broadcast(sseMessage{Event: eventLog, Data: string(data)})
	}
}

func (s *Server) snapshot() stateSnapshot {
	state, stateErr := s.currentState()
	return stateSnapshot{
		Version:          appVersion,
		State:            state,
		Error:            stateErr,
		Phone:            maskPhone(s.client.Phone()),
		TargetChat:       s.client.TargetChat(),
		ActiveTasks:      s.activeTaskCount(),
		Stats:            s.client.Stats(),
		Media:            s.client.ActiveMedia(),
		MediaConcurrency: downloadSettingsDTO{MaxConcurrent: s.client.DownloadConcurrency(), Active: s.client.ActiveDownloadCount()},
		AllPaused:        s.client.AllMediaPaused(),
		SpeedBps:         s.client.DownloadSpeed(),
		Connection:       s.client.ConnectionState(),
	}
}

// activeTaskCount 统计当前非终态（queued/running）的任务数
func (s *Server) activeTaskCount() int {
	tasks := s.queue.List()
	n := 0
	for i := range tasks {
		if tasks[i].Status == string(queue.StatusQueued) || tasks[i].Status == string(queue.StatusRunning) {
			n++
		}
	}
	return n
}

func maskPhone(phone string) string {
	runes := []rune(phone)
	if len(runes) <= phoneMaskKeepHead+phoneMaskKeepTail {
		return phone
	}
	return string(runes[:phoneMaskKeepHead]) +
		strings.Repeat("*", len(runes)-phoneMaskKeepHead-phoneMaskKeepTail) +
		string(runes[len(runes)-phoneMaskKeepTail:])
}

// --- DTOs ---

// appVersion 由 SetVersion 在启动时注入构建版本，经 /api/state 暴露给前端
var appVersion = "dev"

// SetVersion 设置对外展示的应用版本（须在 Run 之前调用）
func SetVersion(v string) {
	if v != "" {
		appVersion = v
	}
}

type stateSnapshot struct {
	Version          string                     `json:"version"`
	State            State                      `json:"state"`
	Error            string                     `json:"error,omitempty"`
	Phone            string                     `json:"phone"`
	TargetChat       int64                      `json:"target_chat"`
	ActiveTasks      int                        `json:"active_tasks"`
	Stats            downloader.Stats           `json:"stats"`
	Media            []downloader.MediaProgress `json:"media"`
	MediaConcurrency downloadSettingsDTO        `json:"media_concurrency"`
	AllPaused        bool                       `json:"all_paused"`
	SpeedBps         int64                      `json:"speed_bps"`
	// Connection 是 TDLib 的网络连接状态（ready/connecting/updating/waiting_network），
	// 让界面能解释"进度条不动"是因为断网，而不是程序卡死
	Connection string `json:"connection,omitempty"`
}

type logEntry struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type downloadSettingsDTO struct {
	MaxConcurrent int `json:"max_concurrent"`
	Active        int `json:"active"`
}
