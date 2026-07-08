// Package telegram provides Telegram client functionality for Tg-Down application.
// It wraps the official TDLib engine (via github.com/zelenin/go-tdlib) and handles
// authentication, chat enumeration, history/live media downloading.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/retry"
)

const (
	// DefaultMessageLimit is the default page size for message queries (TDLib max 100).
	DefaultMessageLimit = 100
	// MessagePreviewLength is the maximum length for message preview text.
	MessagePreviewLength = 50

	// ProgressLogInterval is the byte interval between download progress logs.
	ProgressLogInterval = 8 * 1024 * 1024 // 8MB

	// MaxRenameRetries is the maximum number of file move retry attempts.
	MaxRenameRetries = 5
	// RenameSleepDuration is the sleep duration between move retries.
	RenameSleepDuration = 500 * time.Millisecond

	// downloadPriority is the TDLib download priority (1-32).
	downloadPriority = 1
	// chatLoadBatch is the per-call chat-loading batch size.
	chatLoadBatch = 100
	// maxChatLimit is the upper bound passed to getChats (returns all cached chats).
	maxChatLimit = 1 << 20
	// tdlibLogVerbosity keeps TDLib's own logging quiet (1 = errors only).
	tdlibLogVerbosity = 1

	// fallbackTimeout caps any single TDLib request. It must be large because a
	// synchronous downloadFile blocks here until the whole file is fetched.
	fallbackTimeout = 24 * time.Hour
	// metadataTimeout bounds metadata calls (chats/history) so a stuck request fails fast.
	metadataTimeout = 2 * time.Minute
	// emptyHistoryRetries is how many consecutive empty pages end a history sweep.
	// TDLib often returns empty pages while it fetches history from the server
	// (even with OnlyLocal:false), so the budget must tolerate that latency.
	emptyHistoryRetries = 8
	// emptyHistorySleep is the base wait between consecutive empty history pages;
	// the actual wait grows with the empty streak (capped) so slow server-side
	// backfill is not mistaken for end-of-history.
	emptyHistorySleep = 1 * time.Second
	// maxEmptyHistorySleep caps the progressive empty-page backoff.
	maxEmptyHistorySleep = 5 * time.Second
	// scanLogInterval spaces out history-scan progress log lines so long
	// media-sparse stretches still show visible activity without log spam.
	scanLogInterval = 15 * time.Second

	mediaTypePhoto     = "photo"
	mediaTypeDocument  = "document"
	mediaTypeVideo     = "video"
	mediaTypeAnimation = "animation"
	mediaTypeAudio     = "audio"
	mediaTypeVoice     = "voice"

	copyBufferSize = 1 << 20 // 1MB copy buffer for cross-device fallback

	// dbDirPerm 是 TDLib 数据库目录权限
	dbDirPerm = 0o700
	// tdNotFoundCode 是 TDLib 列表耗尽时返回的错误码
	tdNotFoundCode = 404
	// logoutCloseTimeout 是 LogOut 后等待 TDLib 销毁本地数据并进入 closed 状态的上限
	logoutCloseTimeout = 10 * time.Second
)

// appVersion 上报给 TDLib 的设备/应用版本，由 SetAppVersion 在启动时注入构建版本
var appVersion = "dev"

// SetAppVersion 设置上报给 TDLib 的应用版本（须在 NewClient/Connect 之前调用）
func SetAppVersion(v string) {
	if v != "" {
		appVersion = v
	}
}

// CodeFunc 提供登录验证码
type CodeFunc func(ctx context.Context) (string, error)

// PasswordFunc 提供两步验证密码
type PasswordFunc func(ctx context.Context) (string, error)

// ChatInfo 聊天信息
type ChatInfo struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// Client 是基于 TDLib 的 Telegram 客户端包装器
type Client struct {
	config     *config.Config
	logger     *logger.Logger
	downloader *downloader.Downloader
	retrier    *retry.Retrier

	dbDir    string // TDLib 数据库/会话目录
	filesDir string // TDLib 文件缓存目录（与下载目录同盘，便于 rename）

	targetChatID  atomic.Int64 // 实时监控目标（0 = 不监控）
	monitorTaskID atomic.Value // 实时监控关联的任务ID（string，""=无关联任务）

	mu       sync.Mutex
	td       *tdclient.Client // Connect 后才有值
	closedCh chan struct{}    // Logout 前注册，TDLib 发布 authorizationStateClosed 时关闭

	credMu sync.Mutex // 保护 config.API 凭据（Web 端可动态注入）

	trackMu   sync.Mutex
	fileTrack map[int32]*fileProgress // TDLib file id -> 进度信息（用于日志）

	scanProgressFunc func(taskID string, scannedMessages, foundMedia, scanCursor int64) // 历史扫描进度回调（启动时注册，无并发写）
}

// fileProgress 跟踪单个文件的下载进度（仅用于日志输出）
type fileProgress struct {
	name    string
	total   int64
	lastLog int64
}

// New 创建新的 Telegram 客户端（不监控）
func New(cfg *config.Config, log *logger.Logger) *Client {
	return newClient(cfg, log, 0)
}

// NewWithUpdates 创建带实时监控目标的客户端
func NewWithUpdates(cfg *config.Config, log *logger.Logger, chatID int64) *Client {
	return newClient(cfg, log, chatID)
}

func newClient(cfg *config.Config, log *logger.Logger, chatID int64) *Client {
	c := &Client{
		config:    cfg,
		logger:    log,
		dbDir:     filepath.Join(cfg.Session.Dir, "tdlib"),
		filesDir:  filepath.Join(cfg.Download.Path, ".tdlib-files"),
		fileTrack: make(map[int32]*fileProgress),
		retrier: retry.NewDefault(log).
			WithMaxRetries(cfg.Retry.MaxRetries).
			WithBaseDelay(time.Duration(cfg.Retry.BaseDelay) * time.Second).
			WithMaxDelay(time.Duration(cfg.Retry.MaxDelay) * time.Second),
	}
	c.targetChatID.Store(chatID)
	c.monitorTaskID.Store("")
	c.downloader = downloader.New(cfg.Download.Path, cfg.Download.MaxConcurrent, log)
	c.downloader.SetDownloadFunc(c.DownloadFile)
	c.downloader.SetPauseFunc(c.pauseDownloadFile)
	c.downloader.SetClassifyByType(!cfg.Download.DisableClassifyByType)
	c.downloader.SetSaveMetadata(cfg.Download.SaveMetadata)
	return c
}

// --- 连接与认证 ---

// Authenticate 通过终端交互连接并认证（CLI 模式）
func (c *Client) Authenticate(ctx context.Context) error {
	return c.Connect(ctx, scanlnCode, scanlnPassword)
}

// AuthenticateWith 通过回调连接并认证（Web 模式注入 channel 回调）
func (c *Client) AuthenticateWith(ctx context.Context, codeFn CodeFunc, passwordFn PasswordFunc) error {
	return c.Connect(ctx, codeFn, passwordFn)
}

// Connect 创建 TDLib 客户端并驱动认证，直到授权完成或失败。
// TDLib 在授权后会自行维持/重连，无需外层重连循环。
func (c *Client) Connect(ctx context.Context, codeFn CodeFunc, passwordFn PasswordFunc) error {
	if codeFn == nil {
		codeFn = scanlnCode
	}
	if passwordFn == nil {
		passwordFn = scanlnPassword
	}
	if err := os.MkdirAll(c.dbDir, dbDirPerm); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}

	c.credMu.Lock()
	apiID, apiHash, phone := c.config.API.ID, c.config.API.Hash, c.config.API.Phone
	c.credMu.Unlock()

	params := &tdclient.SetTdlibParametersRequest{
		UseTestDc:           false,
		DatabaseDirectory:   c.dbDir,
		FilesDirectory:      c.filesDir,
		UseFileDatabase:     true,
		UseChatInfoDatabase: true,
		UseMessageDatabase:  true,
		UseSecretChats:      false,
		ApiId:               int32(apiID), //nolint:gosec // api_id 由 Telegram 分配，远小于 int32 上限
		ApiHash:             apiHash,
		SystemLanguageCode:  "en",
		DeviceModel:         "Tg-Down",
		SystemVersion:       appVersion,
		ApplicationVersion:  appVersion,
	}

	handler := &authHandler{c: c, params: params, phone: phone, codeFn: codeFn, passwordFn: passwordFn, ctx: ctx}

	_, _ = tdclient.SetLogVerbosityLevel(&tdclient.SetLogVerbosityLevelRequest{NewVerbosityLevel: tdlibLogVerbosity})

	c.logger.Info("正在连接 Telegram (TDLib)...")
	td, err := tdclient.NewClient(handler,
		tdclient.WithResultHandler(tdclient.NewCallbackResultHandler(c.onUpdate)),
		tdclient.WithFallbackTimeout(fallbackTimeout),
	)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("TDLib 连接/认证失败: %w", err)
	}

	c.mu.Lock()
	c.td = td
	c.mu.Unlock()

	c.checkTDLibVersion(ctx)
	c.logger.Info("Telegram 已连接 (TDLib)")
	return nil
}

// Close 关闭 TDLib 客户端
func (c *Client) Close() {
	c.mu.Lock()
	td := c.td
	c.td = nil
	c.mu.Unlock()
	if td != nil {
		_, _ = td.Close(context.Background())
	}
}

func (c *Client) client() *tdclient.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.td
}

// tdCall 在后台 goroutine 中以不可取消的 background ctx 执行一次 TDLib 请求，规避
// go-tdlib 绑定的 Send 在传入 ctx 取消时 close(catcher) 与接收 goroutine
// 并发发送引发的 "send on closed channel" 进程级崩溃（上游 issue #161，
// 截至 master 0dd3ea6 / 2026-07 复核仍未修复，升级绑定时需重新确认）。
// 本函数仍在 ctx 取消或超时后即时返回；后台 goroutine 会在 TDLib 最终响应
// （或 Close 中止）后自然退出，其响应被安全丢弃。
func tdCall[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	type outcome struct {
		v   T
		err error
	}
	ch := make(chan outcome, 1)
	go func() { // #nosec G118 -- 有意脱离请求 ctx：见函数注释，规避绑定的并发崩溃
		v, err := fn(context.Background())
		ch <- outcome{v, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case o := <-ch:
		return o.v, o.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-timer.C:
		var zero T
		return zero, fmt.Errorf("TDLib 请求超时: %s", timeout)
	}
}

// checkTDLibVersion 比对运行时 TDLib commit 与绑定生成代码所基于的版本
func (c *Client) checkTDLibVersion(ctx context.Context) {
	td := c.client()
	if td == nil {
		return
	}
	opt, err := td.GetOption(&tdclient.GetOptionRequest{Name: "commit_hash"})
	if err != nil {
		return
	}
	if v, ok := opt.(*tdclient.OptionValueString); ok {
		if v.Value != tdclient.TDLIB_VERSION {
			c.logger.Warn("TDLib 版本不匹配: 运行时 %s, 绑定期望 %s（可能导致解析错误）", v.Value, tdclient.TDLIB_VERSION)
		} else {
			c.logger.Debug("TDLib commit: %s", v.Value)
		}
	}
	_ = ctx
}

// IsAuthorized 返回当前是否已授权
func (c *Client) IsAuthorized(ctx context.Context) (bool, error) {
	td := c.client()
	if td == nil {
		return false, nil
	}
	state, err := td.GetAuthorizationState(ctx)
	if err != nil {
		return false, fmt.Errorf("检查授权状态失败: %w", err)
	}
	return state.AuthorizationStateConstructor() == tdclient.ConstructorAuthorizationStateReady, nil
}

// authHandler 实现 tdclient.AuthorizationStateHandler，桥接配置手机号与验证码/密码回调
type authHandler struct {
	c          *Client
	params     *tdclient.SetTdlibParametersRequest
	phone      string
	codeFn     CodeFunc
	passwordFn PasswordFunc
	ctx        context.Context
}

func (h *authHandler) Handle(td *tdclient.Client, state tdclient.AuthorizationState) error {
	switch state.AuthorizationStateConstructor() {
	case tdclient.ConstructorAuthorizationStateWaitTdlibParameters:
		_, err := td.SetTdlibParameters(h.ctx, h.params)
		return err

	case tdclient.ConstructorAuthorizationStateWaitPhoneNumber:
		h.c.logger.Info("提交手机号进行登录...")
		_, err := td.SetAuthenticationPhoneNumber(h.ctx, &tdclient.SetAuthenticationPhoneNumberRequest{PhoneNumber: h.phone})
		return err

	case tdclient.ConstructorAuthorizationStateWaitCode:
		return h.submitCode(td)

	case tdclient.ConstructorAuthorizationStateWaitPassword:
		return h.submitPassword(td)

	case tdclient.ConstructorAuthorizationStateReady,
		tdclient.ConstructorAuthorizationStateClosing,
		tdclient.ConstructorAuthorizationStateClosed:
		return nil

	default:
		return tdclient.NotSupportedAuthorizationState(state)
	}
}

// submitCode 读取并校验验证码；验证码错误时原地重试，不断开连接
func (h *authHandler) submitCode(td *tdclient.Client) error {
	for {
		code, err := h.codeFn(h.ctx)
		if err != nil {
			return fmt.Errorf("读取验证码失败: %w", err)
		}
		_, err = td.CheckAuthenticationCode(h.ctx, &tdclient.CheckAuthenticationCodeRequest{Code: code})
		if err == nil {
			return nil
		}
		if h.ctx.Err() != nil {
			return h.ctx.Err()
		}
		h.c.logger.Warn("验证码错误，请重试: %v", err)
	}
}

// submitPassword 读取并校验两步验证密码；错误时原地重试
func (h *authHandler) submitPassword(td *tdclient.Client) error {
	for {
		pw, err := h.passwordFn(h.ctx)
		if err != nil {
			return fmt.Errorf("读取密码失败: %w", err)
		}
		_, err = td.CheckAuthenticationPassword(h.ctx, &tdclient.CheckAuthenticationPasswordRequest{Password: pw})
		if err == nil {
			h.c.logger.Info("两步验证成功")
			return nil
		}
		if h.ctx.Err() != nil {
			return h.ctx.Err()
		}
		h.c.logger.Warn("两步验证密码错误，请重试: %v", err)
	}
}

func (h *authHandler) Close() {}

// scanlnCode 从终端读取验证码
func scanlnCode(_ context.Context) (string, error) {
	fmt.Printf("请输入验证码: ")
	var code string
	if _, err := fmt.Scanln(&code); err != nil {
		return "", err
	}
	return code, nil
}

// scanlnPassword 从终端读取两步验证密码
func scanlnPassword(_ context.Context) (string, error) {
	fmt.Printf("请输入两步验证密码: ")
	var password string
	if _, err := fmt.Scanln(&password); err != nil {
		return "", err
	}
	return password, nil
}

// --- 监控目标 / 统计 / 会话 ---

// SetTargetChat 设置实时监控目标（0 表示停止监控）
func (c *Client) SetTargetChat(chatID int64) { c.targetChatID.Store(chatID) }

// TargetChat 返回当前监控目标聊天ID
func (c *Client) TargetChat() int64 { return c.targetChatID.Load() }

// SetMonitorTask 设置当前监控任务ID并切换监控目标会话；taskID 为空字符串表示当前无关联任务
func (c *Client) SetMonitorTask(taskID string, chatID int64) {
	c.monitorTaskID.Store(taskID)
	c.SetTargetChat(chatID)
}

// monitorTask 返回当前监控关联的任务ID
func (c *Client) monitorTask() string {
	taskID, _ := c.monitorTaskID.Load().(string)
	return taskID
}

// Stats 返回下载统计快照
func (c *Client) Stats() downloader.Stats { return c.downloader.Snapshot() }

// ActiveMedia 返回当前排队或下载中的媒体进度快照。
func (c *Client) ActiveMedia() []downloader.MediaProgress { return c.downloader.ActiveMedia() }

// PauseMedia 暂停单个媒体下载。
func (c *Client) PauseMedia(ctx context.Context, id string) error {
	return c.downloader.PauseMedia(ctx, id)
}

// ResumeMedia 继续单个媒体下载。
func (c *Client) ResumeMedia(id string) error {
	return c.downloader.ResumeMedia(id)
}

// PauseAllMedia 暂停全部媒体下载，暂停期间新入队的媒体以暂停态开始。
func (c *Client) PauseAllMedia(ctx context.Context) { c.downloader.PauseAll(ctx) }

// ResumeAllMedia 解除全局暂停并继续全部已暂停的媒体。
func (c *Client) ResumeAllMedia() { c.downloader.ResumeAll() }

// AllMediaPaused 返回全局暂停闸状态。
func (c *Client) AllMediaPaused() bool { return c.downloader.AllPaused() }

// DownloadSpeed 返回当前聚合下载速度（字节/秒）。
func (c *Client) DownloadSpeed() int64 { return c.downloader.SpeedBps() }

// DownloadConcurrency 返回当前媒体文件并发下载数量。
func (c *Client) DownloadConcurrency() int { return c.downloader.MaxConcurrent() }

// ActiveDownloadCount 返回正在占用下载槽的媒体数量。
func (c *Client) ActiveDownloadCount() int { return c.downloader.ActiveCount() }

// DownloadPath 返回媒体下载目录
func (c *Client) DownloadPath() string { return c.config.Download.Path }

// ClassifyByType 返回是否按媒体类型分类存储
func (c *Client) ClassifyByType() bool { return c.downloader.ClassifyByType() }

// SetClassifyByType 切换按媒体类型分类存储（立即生效），并写回 config.yaml
func (c *Client) SetClassifyByType(on bool) error {
	c.downloader.SetClassifyByType(on)
	c.credMu.Lock()
	c.config.Download.DisableClassifyByType = !on
	c.credMu.Unlock()
	return c.SaveConfig()
}

// SetDownloadConcurrency 调整媒体文件并发下载数量，并写回 config.yaml 便于下次启动沿用。
func (c *Client) SetDownloadConcurrency(n int) error {
	if n <= 0 {
		return fmt.Errorf("并发数量必须大于 0")
	}
	c.downloader.SetMaxConcurrent(n)
	c.credMu.Lock()
	c.config.Download.MaxConcurrent = n
	c.credMu.Unlock()
	return c.SaveConfig()
}

// SetScanProgressFunc 设置历史扫描进度回调；须在 Connect/任务运行前注册
func (c *Client) SetScanProgressFunc(fn func(taskID string, scannedMessages, foundMedia, scanCursor int64)) {
	c.scanProgressFunc = fn
}

// SetRecordFunc 设置下载记录回调，用于持久化下载历史
func (c *Client) SetRecordFunc(fn func(context.Context, downloader.RecordEvent)) {
	c.downloader.SetRecordFunc(fn)
}

// SetDuplicateLookupFunc 设置内容级去重查找回调（按 unique_id 返回既有文件路径）
func (c *Client) SetDuplicateLookupFunc(fn func(ctx context.Context, uniqueID string) (existingPath string, ok bool)) {
	c.downloader.SetDuplicateLookupFunc(fn)
}

// Phone 返回配置的手机号
func (c *Client) Phone() string {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	return c.config.API.Phone
}

// HasCredentials 判断 API 凭据是否齐全
func (c *Client) HasCredentials() bool {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	return c.config.API.ID != 0 && c.config.API.Hash != "" && c.config.API.Phone != ""
}

// SetCredentials 注入 API 凭据（Web 端登录用）；下次 Connect 生效
func (c *Client) SetCredentials(apiID int, apiHash, phone string) {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	c.config.API.ID = apiID
	c.config.API.Hash = apiHash
	c.config.API.Phone = phone
}

// SaveConfig 将当前配置（含凭据）持久化到 config.yaml，便于下次自动登录
func (c *Client) SaveConfig() error {
	c.credMu.Lock()
	defer c.credMu.Unlock()
	return c.config.SaveConfig("config.yaml")
}

// ClearSession 清除 TDLib 会话（删除数据库目录，强制重新登录）
func (c *Client) ClearSession() error {
	c.Close()
	if err := os.RemoveAll(c.dbDir); err != nil {
		return fmt.Errorf("清除会话失败: %w", err)
	}
	c.logger.Info("会话已清除，下次启动需要重新登录")
	return nil
}

// ClearPhone 清除手机号并写回 config.yaml，使 Web 端回到凭据输入页，
// 且下次启动不会误用旧手机号自动发起登录。
func (c *Client) ClearPhone() error {
	c.credMu.Lock()
	c.config.API.Phone = ""
	c.credMu.Unlock()
	return c.SaveConfig()
}

// Logout 注销当前 Telegram 会话：服务端吊销授权，TDLib 随之销毁本地数据并自行关闭。
// 等待 closed 状态后清理残留会话目录与手机号。注意不可再对已关闭实例调用 Close
// 请求（响应永不到达），仅置空引用。
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	td := c.td
	if td == nil {
		c.mu.Unlock()
		return errors.New("TDLib 未连接")
	}
	closed := make(chan struct{})
	c.closedCh = closed
	c.mu.Unlock()

	if _, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Ok, error) {
		return td.LogOut(cc)
	}); err != nil {
		c.mu.Lock()
		c.closedCh = nil
		c.mu.Unlock()
		return fmt.Errorf("登出失败: %w", err)
	}

	select {
	case <-closed:
	case <-time.After(logoutCloseTimeout):
		c.logger.Warn("等待 TDLib 关闭超时，继续清理本地会话")
	case <-ctx.Done():
	}

	c.mu.Lock()
	if c.td == td {
		c.td = nil
	}
	c.closedCh = nil
	c.mu.Unlock()

	_ = os.RemoveAll(c.dbDir)
	c.SetTargetChat(0)
	c.logger.Info("已退出登录，会话已销毁")
	return c.ClearPhone()
}

// --- 聊天枚举 ---

// GetChats 获取聊天列表（收藏夹置顶 + 主文件夹 + 归档）
func (c *Client) GetChats(ctx context.Context) ([]ChatInfo, error) {
	td := c.client()
	if td == nil {
		return nil, errors.New("TDLib 未连接")
	}

	seen := make(map[int64]bool)
	var order []int64
	for _, list := range []tdclient.ChatList{&tdclient.ChatListMain{}, &tdclient.ChatListArchive{}} {
		c.loadAllChats(ctx, td, list)
		chats, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chats, error) {
			return td.GetChats(cc, &tdclient.GetChatsRequest{ChatList: list, Limit: maxChatLimit})
		})
		if err != nil {
			return nil, fmt.Errorf("获取聊天列表失败: %w", err)
		}
		for _, id := range chats.ChatIds {
			if !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}

	result := make([]ChatInfo, 0, len(order)+1)
	if saved := c.savedMessagesChat(ctx, td); saved != nil {
		result = append(result, *saved)
	}
	for _, id := range order {
		chat, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
			return td.GetChat(cc, &tdclient.GetChatRequest{ChatId: id})
		})
		if err != nil {
			continue
		}
		if info := chatInfoOf(chat); info != nil {
			result = append(result, *info)
		}
	}
	return result, nil
}

// savedMessagesChat 返回收藏夹（Saved Messages，即与自己的私聊）条目；
// chatInfoOf 会过滤所有私聊，故此处显式构建并置顶，即使收藏夹为空或不在聊天列表也可选。失败返回 nil 不阻塞列表
func (c *Client) savedMessagesChat(ctx context.Context, td *tdclient.Client) *ChatInfo {
	me, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.User, error) {
		return td.GetMe(cc)
	})
	if err != nil {
		c.logger.Warn("获取自身账号失败，收藏夹暂不可用: %v", err)
		return nil
	}
	chat, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
		return td.CreatePrivateChat(cc, &tdclient.CreatePrivateChatRequest{UserId: me.Id})
	})
	if err != nil {
		c.logger.Warn("打开收藏夹失败: %v", err)
		return nil
	}
	return &ChatInfo{ID: chat.Id, Title: "收藏夹（Saved Messages）", Type: "收藏夹"}
}

// loadAllChats 反复调用 LoadChats 把指定列表全部载入本地缓存，直到 404（无更多）
func (c *Client) loadAllChats(ctx context.Context, td *tdclient.Client, list tdclient.ChatList) {
	for {
		_, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Ok, error) {
			return td.LoadChats(cc, &tdclient.LoadChatsRequest{ChatList: list, Limit: chatLoadBatch})
		})
		if err == nil {
			continue
		}
		if isNoMoreChats(err) {
			return
		}
		c.logger.Warn("加载聊天列表中断: %v", err)
		return
	}
}

// chatInfoOf 将 TDLib Chat 映射为 ChatInfo（私聊/密聊返回 nil，不在列表展示）
func chatInfoOf(chat *tdclient.Chat) *ChatInfo {
	switch t := chat.Type.(type) {
	case *tdclient.ChatTypeBasicGroup:
		return &ChatInfo{ID: chat.Id, Title: chat.Title, Type: "群组"}
	case *tdclient.ChatTypeSupergroup:
		ty := "超级群组"
		if t.IsChannel {
			ty = "频道"
		}
		return &ChatInfo{ID: chat.Id, Title: chat.Title, Type: ty}
	default:
		return nil
	}
}

// isNoMoreChats 判断 LoadChats 是否因列表已耗尽返回 404
func isNoMoreChats(err error) bool {
	var re tdclient.ResponseError
	if errors.As(err, &re) && re.Err != nil {
		return re.Err.Code == tdNotFoundCode
	}
	return false
}

// --- 媒体提取 ---

// extractMediaInfo 从消息内容提取下载所需的媒体信息（无媒体返回 nil）
func (c *Client) extractMediaInfo(m *tdclient.Message) *downloader.MediaInfo {
	mi := c.extractMediaFile(m)
	if mi == nil {
		return nil
	}
	mi.AlbumID = int64(m.MediaAlbumId)
	mi.Caption = captionText(m.Content)
	mi.SenderID = senderID(m.SenderId)
	return mi
}

// captionText 提取消息内容的 caption 文本（无 caption 的类型返回空串）
func captionText(content tdclient.MessageContent) string {
	var ft *tdclient.FormattedText
	switch c := content.(type) {
	case *tdclient.MessagePhoto:
		ft = c.Caption
	case *tdclient.MessageVideo:
		ft = c.Caption
	case *tdclient.MessageDocument:
		ft = c.Caption
	case *tdclient.MessageAnimation:
		ft = c.Caption
	case *tdclient.MessageAudio:
		ft = c.Caption
	case *tdclient.MessageVoiceNote:
		ft = c.Caption
	}
	if ft == nil {
		return ""
	}
	return ft.Text
}

// senderID 提取发送者的 user/chat id
func senderID(sender tdclient.MessageSender) int64 {
	switch s := sender.(type) {
	case *tdclient.MessageSenderUser:
		return s.UserId
	case *tdclient.MessageSenderChat:
		return s.ChatId
	default:
		return 0
	}
}

// extractMediaFile 按内容类型提取媒体文件信息（不含相册/caption/发送者等消息级字段）
func (c *Client) extractMediaFile(m *tdclient.Message) *downloader.MediaInfo {
	if m == nil || m.Content == nil {
		return nil
	}
	switch content := m.Content.(type) {
	case *tdclient.MessagePhoto:
		return mediaFromFile(m, largestPhotoFile(content.Photo), mediaTypePhoto,
			fmt.Sprintf("photo_%d_%d.jpg", m.ChatId, m.Id), "image/jpeg")
	case *tdclient.MessageDocument:
		if content.Document == nil {
			return nil
		}
		return mediaFromFile(m, content.Document.Document, mediaTypeDocument,
			docName(content.Document.FileName, m.Id), content.Document.MimeType)
	case *tdclient.MessageVideo:
		if content.Video == nil {
			return nil
		}
		return mediaFromFile(m, content.Video.Video, mediaTypeVideo,
			docName(content.Video.FileName, m.Id), content.Video.MimeType)
	case *tdclient.MessageAnimation:
		if content.Animation == nil {
			return nil
		}
		return mediaFromFile(m, content.Animation.Animation, mediaTypeAnimation,
			docName(content.Animation.FileName, m.Id), content.Animation.MimeType)
	case *tdclient.MessageAudio:
		if content.Audio == nil {
			return nil
		}
		return mediaFromFile(m, content.Audio.Audio, mediaTypeAudio,
			docName(content.Audio.FileName, m.Id), content.Audio.MimeType)
	case *tdclient.MessageVoiceNote:
		if content.VoiceNote == nil {
			return nil
		}
		return mediaFromFile(m, content.VoiceNote.Voice, mediaTypeVoice,
			fmt.Sprintf("voice_%d_%d.ogg", m.ChatId, m.Id), content.VoiceNote.MimeType)
	default:
		return nil
	}
}

// mediaFromFile 由 TDLib 文件构建 MediaInfo；file 为 nil 时返回 nil
func mediaFromFile(m *tdclient.Message, f *tdclient.File, mediaType, fileName, mime string) *downloader.MediaInfo {
	if f == nil {
		return nil
	}
	var uniqueID string
	if f.Remote != nil {
		uniqueID = f.Remote.UniqueId
	}
	return &downloader.MediaInfo{
		MessageID: m.Id,
		TDFileID:  f.Id,
		UniqueID:  uniqueID,
		MediaType: mediaType,
		FileName:  fileName,
		FileSize:  fileSize(f),
		MimeType:  mime,
		ChatID:    m.ChatId,
		Date:      time.Unix(int64(m.Date), 0),
	}
}

// largestPhotoFile 返回照片中面积最大的可用 size 对应的文件
func largestPhotoFile(photo *tdclient.Photo) *tdclient.File {
	if photo == nil {
		return nil
	}
	var best *tdclient.PhotoSize
	for _, s := range photo.Sizes {
		if s == nil || s.Photo == nil {
			continue
		}
		if best == nil || int(s.Width)*int(s.Height) >= int(best.Width)*int(best.Height) {
			best = s
		}
	}
	if best == nil {
		return nil
	}
	return best.Photo
}

// docName 以消息ID为前缀生成文件名，保证同名文档在同目录下不互相覆盖
func docName(name string, msgID int64) string {
	if name == "" {
		return fmt.Sprintf("file_%d", msgID)
	}
	return fmt.Sprintf("%d_%s", msgID, name)
}

// fileSize 取文件大小，未知时回退到 ExpectedSize
func fileSize(f *tdclient.File) int64 {
	if f == nil {
		return 0
	}
	if f.Size > 0 {
		return f.Size
	}
	return f.ExpectedSize
}

// --- 下载 ---

// DownloadFile 通过 TDLib 引擎下载文件，完成后从缓存目录移动到目标路径
func (c *Client) DownloadFile(ctx context.Context, media *downloader.MediaInfo, filePath string) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	return c.retrier.Do(ctx, func() error {
		c.logger.Info("下载文件: %s (大小: %d bytes)", media.FileName, media.FileSize)
		c.registerProgress(media)
		defer c.unregisterProgress(media.TDFileID)

		file, err := tdCall(ctx, fallbackTimeout, func(cc context.Context) (*tdclient.File, error) {
			return td.DownloadFile(cc, &tdclient.DownloadFileRequest{
				FileId:      media.TDFileID,
				Priority:    downloadPriority,
				Offset:      0,
				Limit:       0,
				Synchronous: true,
			})
		})
		if err != nil {
			return fmt.Errorf("下载文件失败: %w", err)
		}
		if file.Local == nil || !file.Local.IsDownloadingCompleted || file.Local.Path == "" {
			return fmt.Errorf("下载未完成: %s", media.FileName)
		}
		return c.moveWithRetry(file.Local.Path, filePath)
	})
}

func (c *Client) pauseDownloadFile(ctx context.Context, media *downloader.MediaInfo) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	_, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Ok, error) {
		return td.CancelDownloadFile(cc, &tdclient.CancelDownloadFileRequest{
			FileId:        media.TDFileID,
			OnlyIfPending: false,
		})
	})
	return err
}

// moveWithRetry 将 TDLib 缓存文件移动到目标路径，跨设备时回退为复制
func (c *Client) moveWithRetry(src, dst string) error {
	var err error
	for attempt := 0; attempt < MaxRenameRetries; attempt++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		c.logger.Warn("移动文件失败 (尝试 %d): %v", attempt+1, err)
		time.Sleep(RenameSleepDuration)
	}
	// 回退：跨设备无法 rename，改为复制后删除源文件
	if copyErr := copyFile(src, dst); copyErr != nil {
		return fmt.Errorf("移动文件失败: %w", copyErr)
	}
	_ = os.Remove(src)
	return nil
}

// copyFile 复制文件内容到目标路径。为避免出错时在最终路径留下截断文件
// （后续 os.Stat 存在性检查会将其误判为已下载完成），先写入同目录 .part 临时文件，
// 全部成功后再原子 rename 到目标路径；任何环节失败都会清理临时文件。
func copyFile(src, dst string) error {
	in, err := os.Open(filepath.Clean(src)) // #nosec G304 -- src 为 TDLib 缓存内部路径
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp := filepath.Clean(dst) + ".part"
	out, err := os.Create(tmp) // #nosec G304 -- tmp 由内部下载计划路径派生
	if err != nil {
		return err
	}

	buf := make([]byte, copyBufferSize)
	if _, err = io.CopyBuffer(out, in, buf); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, filepath.Clean(dst)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// registerProgress 注册一个进行中的下载，便于 UpdateFile 输出友好进度日志
func (c *Client) registerProgress(media *downloader.MediaInfo) {
	c.trackMu.Lock()
	c.fileTrack[media.TDFileID] = &fileProgress{name: media.FileName, total: media.FileSize}
	c.trackMu.Unlock()
}

func (c *Client) unregisterProgress(fileID int32) {
	c.trackMu.Lock()
	delete(c.fileTrack, fileID)
	c.trackMu.Unlock()
}

// historyCountFilters 将媒体类型映射到服务端计数过滤器，与 extractMediaInfo 支持的类型一一对应
var historyCountFilters = map[string]tdclient.SearchMessagesFilter{
	mediaTypePhoto:     &tdclient.SearchMessagesFilterPhoto{},
	mediaTypeVideo:     &tdclient.SearchMessagesFilterVideo{},
	mediaTypeDocument:  &tdclient.SearchMessagesFilterDocument{},
	mediaTypeAudio:     &tdclient.SearchMessagesFilterAudio{},
	mediaTypeVoice:     &tdclient.SearchMessagesFilterVoiceNote{},
	mediaTypeAnimation: &tdclient.SearchMessagesFilterAnimation{},
}

// SendSelfMessage 向自己的 Saved Messages 发送一条文本消息（用于任务完成通知）
func (c *Client) SendSelfMessage(ctx context.Context, text string) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	me, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.User, error) {
		return td.GetMe(cc)
	})
	if err != nil {
		return fmt.Errorf("获取当前用户失败: %w", err)
	}
	chat, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
		return td.CreatePrivateChat(cc, &tdclient.CreatePrivateChatRequest{UserId: me.Id})
	})
	if err != nil {
		return fmt.Errorf("打开 Saved Messages 失败: %w", err)
	}
	_, err = tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Message, error) {
		return td.SendMessage(cc, &tdclient.SendMessageRequest{
			ChatId: chat.Id,
			InputMessageContent: &tdclient.InputMessageText{
				Text: &tdclient.FormattedText{Text: text},
			},
		})
	})
	if err != nil {
		return fmt.Errorf("发送通知消息失败: %w", err)
	}
	return nil
}

// ResolvedTarget 是 t.me 链接/公开用户名的解析结果；MessageID 非 0 表示指向单条消息
type ResolvedTarget struct {
	ChatID    int64  `json:"chat_id"`
	Title     string `json:"chat_title"`
	MessageID int64  `json:"message_id,omitempty"`
}

// ResolveTarget 解析下载目标：支持 @用户名、t.me/<name>、t.me/<name>/<msg>、
// t.me/c/<id>/<msg> 及带 https:// 前缀的等价形式。私有链接要求当前账号可访问该聊天。
func (c *Client) ResolveTarget(ctx context.Context, input string) (ResolvedTarget, error) {
	td := c.client()
	if td == nil {
		return ResolvedTarget{}, errors.New("TDLib 未连接")
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return ResolvedTarget{}, errors.New("目标不能为空")
	}

	normalized := strings.TrimPrefix(strings.TrimPrefix(input, "https://"), "http://")
	if path, ok := strings.CutPrefix(normalized, "t.me/"); ok {
		if strings.HasPrefix(path, "+") || strings.HasPrefix(path, "joinchat/") {
			return ResolvedTarget{}, errors.New("暂不支持邀请链接，请先加入该聊天后从列表选择")
		}
		// 含消息序号（t.me/<name>/<msg> 或 t.me/c/<id>/<msg>）走消息链接解析
		if strings.Contains(path, "/") {
			return c.resolveMessageLink(ctx, td, "https://t.me/"+path)
		}
		return c.resolvePublicChat(ctx, td, path)
	}
	return c.resolvePublicChat(ctx, td, strings.TrimPrefix(input, "@"))
}

// resolveMessageLink 经 GetMessageLinkInfo 解析消息链接
func (c *Client) resolveMessageLink(ctx context.Context, td *tdclient.Client, url string) (ResolvedTarget, error) {
	info, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.MessageLinkInfo, error) {
		return td.GetMessageLinkInfo(cc, &tdclient.GetMessageLinkInfoRequest{Url: url})
	})
	if err != nil {
		return ResolvedTarget{}, fmt.Errorf("解析消息链接失败: %w", err)
	}
	if info.ChatId == 0 {
		return ResolvedTarget{}, errors.New("无法访问该链接指向的聊天（可能需要先加入）")
	}
	target := ResolvedTarget{ChatID: info.ChatId}
	if info.Message != nil {
		target.MessageID = info.Message.Id
	}
	if chat, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
		return td.GetChat(cc, &tdclient.GetChatRequest{ChatId: info.ChatId})
	}); err == nil {
		target.Title = chat.Title
	}
	return target, nil
}

// resolvePublicChat 按公开用户名解析聊天
func (c *Client) resolvePublicChat(ctx context.Context, td *tdclient.Client, username string) (ResolvedTarget, error) {
	if username == "" {
		return ResolvedTarget{}, errors.New("用户名不能为空")
	}
	chat, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
		return td.SearchPublicChat(cc, &tdclient.SearchPublicChatRequest{Username: username})
	})
	if err != nil {
		return ResolvedTarget{}, fmt.Errorf("找不到公开聊天 @%s: %w", username, err)
	}
	return ResolvedTarget{ChatID: chat.Id, Title: chat.Title}, nil
}

// CountHistoryMedia 统计聊天历史中可下载媒体的总数（服务端近似值）。
// mediaTypes 非空时只统计选中的类型；日期/大小过滤无法在服务端预估，结果为上估。
// 单个过滤器失败仅跳过；全部失败返回错误，调用方回退为未知总数。
func (c *Client) CountHistoryMedia(ctx context.Context, chatID int64, mediaTypes []string) (int64, error) {
	td := c.client()
	if td == nil {
		return 0, errors.New("TDLib 未连接")
	}
	selected := make([]tdclient.SearchMessagesFilter, 0, len(historyCountFilters))
	if len(mediaTypes) == 0 {
		for _, f := range historyCountFilters {
			selected = append(selected, f)
		}
	} else {
		for _, t := range mediaTypes {
			if f, ok := historyCountFilters[t]; ok {
				selected = append(selected, f)
			}
		}
	}
	var total int64
	succeeded := 0
	for _, filter := range selected {
		f := filter
		cnt, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Count, error) {
			return td.GetChatMessageCount(cc, &tdclient.GetChatMessageCountRequest{
				ChatId:      chatID,
				Filter:      f,
				ReturnLocal: false,
			})
		})
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			c.logger.Warn("统计媒体数量失败 (%s): %v", f.SearchMessagesFilterConstructor(), err)
			continue
		}
		if cnt.Count < 0 { // -1 = 未知
			continue
		}
		total += int64(cnt.Count)
		succeeded++
	}
	if succeeded == 0 {
		return 0, fmt.Errorf("无法统计聊天 %d 的媒体总数", chatID)
	}
	return total, nil
}

// DownloadHistoryMedia 按 spec 下载聊天历史媒体：整聊天任务从游标续扫并流水线分发下载，
// 单消息任务（spec.MessageID != 0）只下载指定消息；恢复任务先补下被重启清扫的中断行
func (c *Client) DownloadHistoryMedia(ctx context.Context, spec *downloader.HistorySpec) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	if spec.FromMessageID > 0 {
		c.logger.Info("继续下载聊天 %d 的历史媒体文件（游标 %d）", spec.ChatID, spec.FromMessageID)
	} else {
		c.logger.Info("开始下载聊天 %d 的历史媒体文件", spec.ChatID)
	}

	closeChat, err := c.openChatForHistory(ctx, td, spec.ChatID)
	if err != nil {
		return err
	}
	if closeChat != nil {
		defer closeChat()
	}

	// 扫描与下载流水线：扫描 goroutine 持续翻页发现媒体并立即分发下载，
	// sem 限制扫描最多领先下载 partitionSize 个在途媒体（内存与队列长度上界）
	partitionSize := c.config.Download.PartitionSize
	if partitionSize <= 0 {
		partitionSize = config.DefaultPartitionSize
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, partitionSize)

	// dispatch 将单个媒体投入下载流水线（受 sem 在途上限约束）
	dispatch := func(mi *downloader.MediaInfo) error {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.downloader.DownloadMedia(ctx, mi); err != nil {
				c.logger.Error("下载媒体文件失败: %v", err)
			}
		}()
		return nil
	}

	// 单消息任务（t.me 消息链接）：只下载指定消息，不扫描历史
	if spec.MessageID != 0 {
		err := c.downloadSingleHistoryMessage(ctx, td, spec, dispatch)
		wg.Wait()
		if err != nil {
			return err
		}
		c.downloader.PrintStats()
		return nil
	}

	// 恢复任务先补下被重启清扫的中断行：这些消息比游标更新，续扫不会再经过
	if len(spec.RetryMessageIDs) > 0 {
		if err := c.retryInterruptedMessages(ctx, td, spec, dispatch); err != nil {
			wg.Wait()
			return err
		}
	}

	scannedMessages, foundMedia, scanErr := c.scanHistoryPages(ctx, td, spec, dispatch)
	if scanErr == nil {
		c.logger.Info("历史扫描完成: 共扫描 %d 条消息，发现 %d 个媒体", scannedMessages, foundMedia)
	}
	// 扫描出错或取消时也等在途下载全部退出，避免任务进入终态后仍有下载在更新统计
	wg.Wait()
	if scanErr != nil {
		return scanErr
	}

	c.logger.Info("历史媒体文件下载完成，总计处理 %d 个文件", foundMedia)
	c.downloader.PrintStats()
	return nil
}

// openChatForHistory 校验聊天可访问并打开聊天，促使 TDLib 主动从服务器同步历史；
// 冷缓存时首批 GetChatHistory 常为空，否则可能在历史尚未拉取就误判"已完成"。
// 返回的 closeFn（可为 nil）应在拉取结束后调用以释放 TDLib 资源。
func (c *Client) openChatForHistory(ctx context.Context, td *tdclient.Client, chatID int64) (closeFn func(), err error) {
	if _, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
		return td.GetChat(cc, &tdclient.GetChatRequest{ChatId: chatID})
	}); err != nil {
		return nil, fmt.Errorf("无法访问聊天 %d: %w", chatID, err)
	}

	if _, openErr := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Ok, error) {
		return td.OpenChat(cc, &tdclient.OpenChatRequest{ChatId: chatID})
	}); openErr != nil {
		c.logger.Warn("打开聊天失败（继续尝试拉取历史）: %v", openErr)
		return nil, nil
	}
	return func() {
		_, _ = td.CloseChat(context.Background(), &tdclient.CloseChatRequest{ChatId: chatID})
	}, nil
}

// scanHistoryPages 从 spec.FromMessageID 起向更旧方向翻页扫描，按任务过滤器筛选并分发下载；
// 游标随页推进经 reportScanProgress 上报持久化
func (c *Client) scanHistoryPages(
	ctx context.Context, td *tdclient.Client, spec *downloader.HistorySpec,
	dispatch func(*downloader.MediaInfo) error,
) (scannedMessages, foundMedia int64, err error) {
	batchSize := c.config.Download.BatchSize
	if batchSize <= 0 || batchSize > DefaultMessageLimit {
		batchSize = DefaultMessageLimit
	}
	limit := int32(batchSize) // 已上界钳制到 DefaultMessageLimit(100)，不会溢出

	fromMsgID := spec.FromMessageID // 0 = 从最新开始；>0 = 断点续扫
	emptyStreak := 0
	lastScanLog := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return scannedMessages, foundMedia, err
		}

		pageMsgs, err := fetchHistoryPage(ctx, td, spec.ChatID, fromMsgID, limit)
		if err != nil {
			return scannedMessages, foundMedia, err
		}

		if len(pageMsgs) == 0 {
			emptyStreak++
			stop, err := awaitNextHistoryPage(ctx, emptyStreak)
			if err != nil || stop {
				return scannedMessages, foundMedia, err
			}
			continue
		}
		emptyStreak = 0

		media, lastMsgID, pastDateFrom := c.extractBatchMedia(pageMsgs, spec.TaskID, spec.Filters)
		fromMsgID = lastMsgID // 推进到本页最旧消息
		scannedMessages += int64(len(pageMsgs))
		foundMedia += int64(len(media))
		// 游标随页推进即上报持久化；本页/在途媒体若在落盘后被杀，
		// 由启动清扫（interrupted）+ 恢复补下（RetryMessageIDs）兜底，不会漏
		c.reportScanProgress(spec.TaskID, scannedMessages, foundMedia, fromMsgID)

		c.downloader.PlanBatch(media)
		for _, m := range media {
			if err := dispatch(m); err != nil {
				return scannedMessages, foundMedia, err
			}
		}
		if pastDateFrom {
			c.logger.Info("历史扫描已越过起始日期，提前结束")
			return scannedMessages, foundMedia, nil
		}

		if time.Since(lastScanLog) >= scanLogInterval {
			c.logger.Info("扫描进度: 已扫描 %d 条消息，发现 %d 个媒体", scannedMessages, foundMedia)
			lastScanLog = time.Now()
		}
	}
}

// retryInterruptedMessages 逐条重取并补下恢复任务的中断消息；消息已删除或无媒体时记警告跳过
func (c *Client) retryInterruptedMessages(
	ctx context.Context, td *tdclient.Client, spec *downloader.HistorySpec,
	dispatch func(*downloader.MediaInfo) error,
) error {
	var batch []*downloader.MediaInfo
	for _, msgID := range spec.RetryMessageIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Message, error) {
			return td.GetMessage(cc, &tdclient.GetMessageRequest{ChatId: spec.ChatID, MessageId: msgID})
		})
		if err != nil {
			c.logger.Warn("补下中断媒体失败（消息 %d 可能已删除）: %v", msgID, err)
			continue
		}
		if media := c.extractMediaInfo(msg); media != nil &&
			spec.Filters.Match(media.MediaType, int64(msg.Date), media.FileSize) {
			media.TaskID = spec.TaskID
			batch = append(batch, media)
		}
	}
	c.downloader.PlanBatch(batch)
	for _, m := range batch {
		if err := dispatch(m); err != nil {
			return err
		}
	}
	return nil
}

// downloadSingleHistoryMessage 下载单条消息的媒体（t.me 消息链接任务）
func (c *Client) downloadSingleHistoryMessage(
	ctx context.Context, td *tdclient.Client, spec *downloader.HistorySpec,
	dispatch func(*downloader.MediaInfo) error,
) error {
	msg, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Message, error) {
		return td.GetMessage(cc, &tdclient.GetMessageRequest{ChatId: spec.ChatID, MessageId: spec.MessageID})
	})
	if err != nil {
		return fmt.Errorf("获取消息 %d 失败: %w", spec.MessageID, err)
	}
	media := c.extractMediaInfo(msg)
	if media == nil {
		return fmt.Errorf("消息 %d 不包含可下载的媒体", spec.MessageID)
	}
	if !spec.Filters.Match(media.MediaType, int64(msg.Date), media.FileSize) {
		return fmt.Errorf("消息 %d 的媒体被任务过滤器排除", spec.MessageID)
	}
	media.TaskID = spec.TaskID
	c.downloader.PlanBatch([]*downloader.MediaInfo{media})
	return dispatch(media)
}

// reportScanProgress 上报历史扫描进度与游标；未注册回调（CLI 模式）或无任务 ID 时静默
func (c *Client) reportScanProgress(taskID string, scannedMessages, foundMedia, scanCursor int64) {
	if c.scanProgressFunc == nil || taskID == "" {
		return
	}
	c.scanProgressFunc(taskID, scannedMessages, foundMedia, scanCursor)
}

// fetchHistoryPage 拉取一页历史消息，并剔除 Offset:0 时 TDLib 附带返回的
// FromMessageId 边界消息本身（非首次请求时），避免重复处理及"仅剩边界消息"导致的死循环
func fetchHistoryPage(ctx context.Context, td *tdclient.Client, chatID, fromMsgID int64, limit int32) ([]*tdclient.Message, error) {
	msgs, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Messages, error) {
		return td.GetChatHistory(cc, &tdclient.GetChatHistoryRequest{
			ChatId:        chatID,
			FromMessageId: fromMsgID,
			Offset:        0,
			Limit:         limit,
			OnlyLocal:     false,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("获取消息历史失败: %w", err)
	}

	pageMsgs := msgs.Messages
	if fromMsgID != 0 && len(pageMsgs) > 0 && pageMsgs[0].Id == fromMsgID {
		pageMsgs = pageMsgs[1:]
	}
	return pageMsgs, nil
}

// extractBatchMedia 从一页历史消息中提取媒体信息、按任务过滤器筛选并打上任务ID；
// 返回本页最旧消息ID供调用方推进下一页起点，以及整页是否已早于 DateFrom（可提前停止翻页：
// 历史页按新到旧返回，整页更旧则更早的页必然全部越界）
func (c *Client) extractBatchMedia(
	msgs []*tdclient.Message, taskID string, filters downloader.HistoryFilters,
) (media []*downloader.MediaInfo, lastMsgID int64, pastDateFrom bool) {
	pastDateFrom = len(msgs) > 0 && filters.DateFrom != 0
	for _, m := range msgs {
		if int64(m.Date) >= filters.DateFrom {
			pastDateFrom = false
		}
		if mi := c.extractMediaInfo(m); mi != nil && filters.Match(mi.MediaType, int64(m.Date), mi.FileSize) {
			mi.TaskID = taskID
			media = append(media, mi)
		}
		lastMsgID = m.Id
	}
	return media, lastMsgID, pastDateFrom
}

// awaitNextHistoryPage 处理获取到空历史页时的退避逻辑：
// 连续空页达到阈值则停止轮询（stop=true）；否则等待后允许继续下一页。
func awaitNextHistoryPage(ctx context.Context, emptyStreak int) (stop bool, err error) {
	if emptyStreak >= emptyHistoryRetries {
		return true, nil
	}
	wait := time.Duration(emptyStreak) * emptyHistorySleep
	if wait > maxEmptyHistorySleep {
		wait = maxEmptyHistorySleep
	}
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-time.After(wait):
	}
	return false, nil
}

// --- TDLib 更新处理 ---

// onUpdate 是 TDLib 结果回调（运行于库的单一接收 goroutine，必须快速非阻塞）
func (c *Client) onUpdate(t tdclient.Type) {
	switch u := t.(type) {
	case *tdclient.UpdateFile:
		c.onUpdateFile(u.File)
	case *tdclient.UpdateNewMessage:
		c.onNewMessage(u.Message)
	case *tdclient.UpdateConnectionState:
		c.onConnectionState(u.State)
	case *tdclient.UpdateAuthorizationState:
		c.onAuthorizationState(u.AuthorizationState)
	}
}

// onAuthorizationState 在 TDLib 进入 closed 状态时通知 Logout 等待方（必须快速非阻塞）
func (c *Client) onAuthorizationState(state tdclient.AuthorizationState) {
	if state == nil || state.AuthorizationStateConstructor() != tdclient.ConstructorAuthorizationStateClosed {
		return
	}
	c.mu.Lock()
	ch := c.closedCh
	c.closedCh = nil
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// onUpdateFile 按字节间隔输出下载进度日志
func (c *Client) onUpdateFile(f *tdclient.File) {
	if f == nil || f.Local == nil {
		return
	}
	done := f.Local.DownloadedSize
	total := f.Size
	if total <= 0 {
		total = f.ExpectedSize
	}
	c.downloader.UpdateProgress(f.Id, done, total, f.Local.IsDownloadingCompleted)

	c.trackMu.Lock()
	fp := c.fileTrack[f.Id]
	var logLine string
	if fp != nil {
		total := fp.total
		if total <= 0 {
			total = f.Size
		}
		if done-fp.lastLog >= ProgressLogInterval || (total > 0 && f.Local.IsDownloadingCompleted) {
			fp.lastLog = done
			if total > 0 {
				logLine = fmt.Sprintf("下载进度 %s: %.1f%% (%d/%d bytes)", fp.name, float64(done)/float64(total)*100, done, total)
			} else {
				logLine = fmt.Sprintf("下载进度 %s: %d bytes", fp.name, done)
			}
		}
	}
	c.trackMu.Unlock()

	if logLine != "" {
		c.logger.Info("%s", logLine)
	}
}

// onNewMessage 实时监控：目标聊天的新媒体消息触发下载
func (c *Client) onNewMessage(m *tdclient.Message) {
	if m == nil {
		return
	}
	target := c.targetChatID.Load()
	if target == 0 || m.ChatId != target {
		return
	}
	media := c.extractMediaInfo(m)
	if media == nil {
		c.logger.Info("📝 目标聊天新消息（无媒体）: %s", messagePreview(m))
		return
	}
	media.TaskID = c.monitorTask()
	c.logger.Info("🎬 检测到目标聊天新媒体: %s", media.FileName)
	go func() { c.downloader.DownloadSingle(context.Background(), media) }()
}

// onConnectionState 输出连接状态变化
func (c *Client) onConnectionState(state tdclient.ConnectionState) {
	if state == nil {
		return
	}
	switch state.(type) {
	case *tdclient.ConnectionStateReady:
		c.logger.Debug("TDLib 连接就绪")
	case *tdclient.ConnectionStateConnecting, *tdclient.ConnectionStateConnectingToProxy:
		c.logger.Debug("TDLib 正在连接...")
	case *tdclient.ConnectionStateWaitingForNetwork:
		c.logger.Warn("TDLib 等待网络...")
	}
}

// messagePreview 生成消息预览文本
func messagePreview(m *tdclient.Message) string {
	if m == nil || m.Content == nil {
		return "[空消息]"
	}
	switch content := m.Content.(type) {
	case *tdclient.MessageText:
		txt := ""
		if content.Text != nil {
			txt = content.Text.Text
		}
		if len(txt) > MessagePreviewLength {
			return txt[:MessagePreviewLength] + "..."
		}
		return txt
	case *tdclient.MessagePhoto:
		return "[图片]"
	case *tdclient.MessageVideo:
		return "[视频]"
	case *tdclient.MessageDocument:
		return "[文档]"
	case *tdclient.MessageAnimation:
		return "[动图]"
	case *tdclient.MessageAudio:
		return "[音频]"
	case *tdclient.MessageVoiceNote:
		return "[语音]"
	default:
		return "[消息]"
	}
}
