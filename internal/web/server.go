// Package web provides a local Apple-styled web management UI for Tg-Down.
// It drives a long-lived Telegram connection, exposes chat browsing, history
// downloads and live monitoring over HTTP, and streams progress via SSE.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/queue"
	"tg-down/internal/store"
	"tg-down/internal/telegram"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/prism.css
var prismCSS []byte

const (
	// DefaultAddr is the default listen address (localhost only).
	DefaultAddr = "127.0.0.1:8080"

	shutdownTimeout       = 5 * time.Second
	readHeaderTimeout     = 10 * time.Second
	snapshotInterval      = time.Second
	sseBufferSize         = 32
	authChanSize          = 1
	phoneMaskKeepHead     = 3
	phoneMaskKeepTail     = 2
	initialReconnectDelay = 2 * time.Second
	maxReconnectDelay     = 30 * time.Second
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
	client  *telegram.Client
	store   *store.Store
	queue   *queue.Manager
	logger  *logger.Logger
	addr    string
	hub     *sseHub
	baseCtx context.Context // 下载任务的生命周期父上下文（在 Run 中设置）

	mu       sync.RWMutex
	state    State
	stateErr string
	chats    []telegram.ChatInfo

	codeCh chan string
	passCh chan string
	credCh chan struct{} // Web 端提交 API 凭据的信号
}

// New 创建 Web 管理端，内部以 maxConcurrentTasks 构建任务队列管理器
func New(client *telegram.Client, st *store.Store, log *logger.Logger, addr string, maxConcurrentTasks int) *Server {
	if addr == "" {
		addr = DefaultAddr
	}
	return &Server{
		client: client,
		store:  st,
		queue:  queue.NewManager(client, st, log, maxConcurrentTasks),
		logger: log,
		addr:   addr,
		hub:    newSSEHub(),
		state:  StateConnecting,
		codeCh: make(chan string, authChanSize),
		passCh: make(chan string, authChanSize),
		credCh: make(chan struct{}, authChanSize),
	}
}

// Run 启动后台 Telegram 连接与 HTTP 服务，阻塞直到 ctx 取消
func (s *Server) Run(ctx context.Context) error {
	s.baseCtx = ctx
	s.logger.SetHook(s.onLog)
	defer s.logger.SetHook(nil)

	s.queue.SetOnChange(s.onTaskChange)
	go s.runTelegram(ctx)
	go s.snapshotLoop(ctx)
	go s.queue.Run(ctx)

	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() { //nolint:gosec // 父 ctx 已取消，关闭需独立超时窗口
		<-ctx.Done()
		// 运行中任务的 ctx 派生自同一个 ctx，取消已沿调用链自动传播，
		// 此处无需再显式遍历取消。
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if !strings.HasPrefix(s.addr, "127.0.0.1") && !strings.HasPrefix(s.addr, "localhost") {
		s.logger.Warn("Web 端监听非本地地址 %s，无访问鉴权，请确保网络可信", s.addr)
	}
	s.logger.Info("Web 管理端已启动: http://%s", s.addr)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务失败: %w", err)
	}
	return nil
}

// runTelegram 连接并认证 Telegram；TDLib 在授权后自行维持/重连，
// 故仅在初始连接失败时退避重试。验证码/密码经 webCode/webPassword 注入。
func (s *Server) runTelegram(ctx context.Context) {
	delay := initialReconnectDelay
	for {
		// 凭据缺失时，等待 Web 端提交 API ID/Hash/手机号
		if !s.client.HasCredentials() {
			s.setState(StateNeedCredentials)
			select {
			case <-s.credCh:
				delay = initialReconnectDelay
			case <-ctx.Done():
				s.client.Close()
				return
			}
		}

		s.setState(StateConnecting)
		err := s.client.AuthenticateWith(ctx, s.webCode, s.webPassword)
		if ctx.Err() != nil {
			s.client.Close()
			return
		}
		if err != nil {
			s.setError(err)
			s.logger.Error("Telegram 连接失败，%s 后重试: %v", delay, err)
			// 退避等待；若用户期间重新提交凭据则立即重试
			select {
			case <-time.After(delay):
				delay = min(delay*reconnectFactor, maxReconnectDelay)
			case <-s.credCh:
				delay = initialReconnectDelay
			case <-ctx.Done():
				s.client.Close()
				return
			}
			continue
		}

		s.setState(StateReady)
		s.logger.Info("Telegram 已连接，Web 端就绪")
		s.refreshChats(ctx)

		<-ctx.Done()
		s.client.Close()
		return
	}
}

// webCode 等待 Web 端提交验证码
func (s *Server) webCode(ctx context.Context) (string, error) {
	s.setState(StateWaitingCode)
	select {
	case code := <-s.codeCh:
		return code, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// webPassword 等待 Web 端提交两步验证密码
func (s *Server) webPassword(ctx context.Context) (string, error) {
	s.setState(StateWaitingPassword)
	select {
	case pw := <-s.passCh:
		return pw, nil
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
		State:       state,
		Error:       stateErr,
		Phone:       maskPhone(s.client.Phone()),
		TargetChat:  s.client.TargetChat(),
		ActiveTasks: s.activeTaskCount(),
		Stats:       s.client.Stats(),
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
	if len(phone) <= phoneMaskKeepHead+phoneMaskKeepTail {
		return phone
	}
	return phone[:phoneMaskKeepHead] + strings.Repeat("*", len(phone)-phoneMaskKeepHead-phoneMaskKeepTail) +
		phone[len(phone)-phoneMaskKeepTail:]
}

// --- DTOs ---

type stateSnapshot struct {
	State       State            `json:"state"`
	Error       string           `json:"error,omitempty"`
	Phone       string           `json:"phone"`
	TargetChat  int64            `json:"target_chat"`
	ActiveTasks int              `json:"active_tasks"`
	Stats       downloader.Stats `json:"stats"`
}

type logEntry struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}
