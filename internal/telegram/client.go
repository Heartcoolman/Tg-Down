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
	"syscall"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"
	"golang.org/x/term"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	mediapkg "tg-down/internal/media"
	"tg-down/internal/retry"
	"tg-down/internal/tgapi"
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

	// downloadPriority is the TDLib download priority (1-32; 数字越大越优先)。
	downloadPriority = 1
	// thumbDownloadPriority 与主文件同为最低档：TDLib 的优先级下界就是 1，
	// 没法把缩略图排得更靠后。它们本身只有几十 KB，抢不走多少带宽。
	thumbDownloadPriority = 1
	// thumbDownloadTimeout 是单个缩略图的下载超时。缩略图很小，
	// 拖久了直接放弃（画廊会退回 minithumb 占位图），不值得为它拖住整个下载。
	thumbDownloadTimeout = 30 * time.Second
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
	// scanLogInterval spaces out history-scan progress log lines so long
	// media-sparse stretches still show visible activity without log spam.
	scanLogInterval = 15 * time.Second

	copyBufferSize = 1 << 20 // 1MB copy buffer for cross-device fallback

	// dbDirPerm 是 TDLib 数据库目录权限
	dbDirPerm = 0o700
	// tdNotFoundCode 是 TDLib 列表耗尽时返回的错误码
	tdNotFoundCode = 404
	// logoutCloseTimeout 是 LogOut 后等待 TDLib 销毁本地数据并进入 closed 状态的上限
	logoutCloseTimeout = 10 * time.Second
	// tdCloseTimeout 限制普通 Close 等待 TDLib 响应的时间。
	tdCloseTimeout = 10 * time.Second

	// maxFloodWait 是可接受的最长限流等待。Telegram 的 FLOOD_WAIT 通常是秒到分钟级；
	// 超过此值说明限流窗口过长，与其占着下载槽空等，不如失败后由任务级重试接手。
	maxFloodWait = 5 * time.Minute

	// minDownloadTimeout 是单文件下载的最小时限（小文件也允许慢速链路下的握手与排队）。
	minDownloadTimeout = 30 * time.Minute
	// downloadMinBytesPerSec 是推算下载时限所假设的最低吞吐（约 20 KB/s）。低于此速度视为卡死。
	downloadMinBytesPerSec = 20 * 1024
)

// downloadTimeout 按文件大小推算单次下载的时限。
//
// 此前所有 TDLib 请求统一用 24 小时的 fallbackTimeout，于是一个卡死的下载会把 goroutine
// 挂住整整一天（tdCall 的后台 goroutine 脱离请求 ctx，取消不会中止底层请求）。按大小推算
// 的时限让卡死的下载有界退出，同时不会误杀慢速链路上的大文件。
func downloadTimeout(size int64) time.Duration {
	if size <= 0 {
		return fallbackTimeout // 大小未知：保持宽松上限，交由 TDLib 自行收敛
	}
	d := time.Duration(size/downloadMinBytesPerSec) * time.Second
	if d < minDownloadTimeout {
		return minDownloadTimeout
	}
	if d > fallbackTimeout {
		return fallbackTimeout
	}
	return d
}

// appVersion 上报给 TDLib 的设备/应用版本，由 SetAppVersion 在启动时注入构建版本
var appVersion = "dev"

// SetAppVersion 设置上报给 TDLib 的应用版本（须在 NewClient/Connect 之前调用）
func SetAppVersion(v string) {
	if v != "" {
		appVersion = v
	}
}

// 以下类型的定义在 internal/tgapi（无 CGo 的叶子包），这里别名再导出：
// web 层只依赖 tgapi，就不会被本包的 TDLib/CGo 依赖传染，其测试无需先编 TDLib。
type (
	// CodeFunc 提供登录验证码
	CodeFunc = tgapi.CodeFunc
	// PasswordFunc 提供两步验证密码
	PasswordFunc = tgapi.PasswordFunc
	// ChatInfo 聊天信息
	ChatInfo = tgapi.ChatInfo
	// ResolvedTarget 是一次目标解析的结果
	ResolvedTarget = tgapi.ResolvedTarget
	// ExportSpec 描述一次聊天导出
	ExportSpec = tgapi.ExportSpec
	// ExportResult 是导出产物的落盘位置与规模
	ExportResult = tgapi.ExportResult
)

// 钉住 *Client 满足 web 层依赖的接口：改动方法签名时在本包就会编译失败，
// 而不是等到 web 包才暴露。
var _ tgapi.Client = (*Client)(nil)

// Client 是基于 TDLib 的 Telegram 客户端包装器
type Client struct {
	config     *config.Config
	logger     *logger.Logger
	downloader *downloader.Downloader
	retrier    *retry.Retrier

	dbDir    string // TDLib 数据库/会话目录
	filesDir string // TDLib 文件缓存目录（与下载目录同盘，便于 rename）

	monitorSwitchMu sync.Mutex // 串行化“停止旧监控并安装新监控”的完整过程
	monitorMu       sync.RWMutex
	monitor         *monitorGeneration // 当前一代监控；锁内整体替换，切换期间没有空状态
	connState       atomic.Value       // TDLib 网络连接状态（string），供 Web 端显示"等待网络"等

	mu       sync.Mutex
	td       tdAPI         // Connect 后才有值；接口类型使本包可在无真实 TDLib 连接时测试
	closedCh chan struct{} // Logout 前注册，TDLib 发布 authorizationStateClosed 时关闭

	credMu sync.Mutex // 保护 config.API 凭据（Web 端可动态注入）

	trackMu   sync.Mutex
	fileTrack map[int32]*fileProgress // TDLib file id -> 进度信息（用于日志）
	attemptMu sync.Mutex
	attempts  map[int32]*downloadAttempt // 正在执行的 TDLib 下载；用户暂停时取消其重试上下文

	scanProgressFunc func(taskID string, scannedMessages, foundMedia, scanCursor int64) // 历史扫描进度回调（启动时注册，无并发写）
}

// monitorState 是一次完整的实时监控配置。新消息处理只读取一个快照，
// 避免切换任务时把旧目标、新任务 ID 和新标题拼在一起。
type monitorState struct {
	chatID    int64
	taskID    string
	chatTitle string
}

type monitorGeneration struct {
	state  monitorState
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type downloadAttempt struct {
	cancel context.CancelFunc
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
		attempts:  make(map[int32]*downloadAttempt),
		retrier: retry.NewDefault(log).
			WithClassifier(tdShouldRetry, tdRetryAfter, maxFloodWait).
			WithMaxRetries(cfg.Retry.MaxRetries).
			WithBaseDelay(time.Duration(cfg.Retry.BaseDelay) * time.Second).
			WithMaxDelay(time.Duration(cfg.Retry.MaxDelay) * time.Second),
	}
	if chatID != 0 {
		c.monitor = newMonitorGeneration(monitorState{chatID: chatID})
	}
	c.downloader = downloader.New(cfg.Download.Path, cfg.Download.MaxConcurrent, log)
	c.downloader.SetDownloadFunc(c.DownloadFile)
	c.downloader.SetPauseFunc(c.pauseDownloadFile)
	c.downloader.SetThumbDownloadFunc(c.DownloadThumbFile)
	c.downloader.SetClassifyByType(!cfg.Download.DisableClassifyByType)
	c.downloader.SetSaveMetadata(cfg.Download.SaveMetadata)
	if tpl := cfg.Download.PathTemplate; tpl != "" {
		if problem := downloader.ValidatePathTemplate(tpl); problem != "" {
			log.Warn("路径模板无效（%s），回退到默认布局: %s", problem, downloader.DefaultPathTemplate)
		}
	}
	c.downloader.SetPathTemplate(cfg.Download.PathTemplate)
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
	c.credMu.Lock()
	credentialsValid := c.config.HasAPICredentials()
	apiID, apiHash, phone := c.config.API.ID, c.config.API.Hash, c.config.API.Phone
	c.credMu.Unlock()
	if !credentialsValid {
		return errors.New("无效的 Telegram API 凭据")
	}
	if err := validateSessionDBPath(c.config.Session.Dir, c.dbDir); err != nil {
		return fmt.Errorf("会话目录不安全: %w", err)
	}
	if err := ensurePrivateDir(c.dbDir); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}
	if err := validateSessionDBPath(c.config.Session.Dir, c.dbDir); err != nil {
		return fmt.Errorf("会话目录不安全: %w", err)
	}

	params := &tdclient.SetTdlibParametersRequest{
		UseTestDc:           false,
		DatabaseDirectory:   c.dbDir,
		FilesDirectory:      c.filesDir,
		UseFileDatabase:     true,
		UseChatInfoDatabase: true,
		UseMessageDatabase:  true,
		UseSecretChats:      false,
		ApiId:               int32(apiID), //nolint:gosec // 已由 config.HasAPICredentials 校验 int32 范围
		ApiHash:             apiHash,
		SystemLanguageCode:  "en",
		DeviceModel:         "Tg-Down",
		SystemVersion:       appVersion,
		ApplicationVersion:  appVersion,
	}

	// 代理必须在首次网络活动前启用：TDLib 不读 HTTP_PROXY/HTTPS_PROXY 等环境变量，
	// 直连被墙的网络会永远停在"正在连接"（issue #49）。解析失败直接报错——
	// 静默回退直连只会让用户重新面对无提示的卡死。
	proxyReq, proxySource, err := resolveTelegramProxy(c.config.Telegram.Proxy)
	if err != nil {
		return err
	}

	handler := &authHandler{c: c, params: params, phone: phone, codeFn: codeFn, passwordFn: passwordFn, ctx: ctx, proxy: proxyReq}

	_, _ = tdclient.SetLogVerbosityLevel(&tdclient.SetLogVerbosityLevelRequest{NewVerbosityLevel: tdlibLogVerbosity})

	c.logger.Info("正在连接 Telegram (TDLib)...")
	if proxyReq != nil {
		c.logger.Info("经代理 %s 连接（%s）", describeProxy(proxyReq), proxySource)
	}

	// 连接停滞提示：迟迟未就绪时给出可操作的排查建议（此前只会无限期静默卡住）
	hintDone := make(chan struct{})
	defer close(hintDone)
	go c.runConnectHint(proxyReq != nil, hintDone)

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

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, dbDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, dbDirPerm)
}

func validateSessionDBPath(sessionDir, dbDir string) error {
	expected, err := filepath.Abs(filepath.Join(sessionDir, "tdlib"))
	if err != nil {
		return err
	}
	actual, err := filepath.Abs(dbDir)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("TDLib 目录不是 session.dir 的直接子目录: %s", dbDir)
	}
	for _, path := range []string{sessionDir, dbDir} {
		if path == "" {
			continue
		}
		if err := rejectUntrustedSymlinkComponents(path); err != nil {
			return err
		}
		info, statErr := os.Lstat(filepath.Clean(path))
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return fmt.Errorf("会话路径不是目录: %s", path)
		}
	}
	return nil
}

// rejectUntrustedSymlinkComponents 从可信的本机路径前缀之后逐段检查已有路径。
// home/cwd/TMPDIR 可能本身经过系统链接（macOS 的 /var 即常见例子），但应用控制的
// 后续目录不允许再通过符号链接跳到别处。
func rejectUntrustedSymlinkComponents(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	base := trustedPathBase(absPath)
	rel, err := filepath.Rel(base, absPath)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := base
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("会话路径包含符号链接: %s", current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("会话路径分量不是目录: %s", current)
		}
	}
	return nil
}

func trustedPathBase(absPath string) string {
	volume := filepath.VolumeName(absPath)
	best := volume + string(filepath.Separator)
	candidates := make([]string, 0, 3)
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	candidates = append(candidates, os.TempDir())
	for _, candidate := range candidates {
		absCandidate, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absCandidate, absPath)
		if err != nil || rel == ".." || filepath.IsAbs(rel) ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(absCandidate) > len(best) {
			best = absCandidate
		}
	}
	return best
}

// RemoveSessionDatabase 安全删除 <sessionDir>/tdlib，供客户端与 --clear-session 共用。
func RemoveSessionDatabase(sessionDir string) error {
	return removeSessionDatabase(sessionDir, filepath.Join(sessionDir, "tdlib"))
}

func removeSessionDatabase(sessionDir, dbDir string) error {
	if err := validateSessionDBPath(sessionDir, dbDir); err != nil {
		return err
	}
	return os.RemoveAll(dbDir)
}

// Close 先停止实时监控下载，再有界关闭 TDLib 客户端。可重复调用。
func (c *Client) Close() {
	c.stopMonitor()
	c.cancelAllDownloadAttempts()
	c.mu.Lock()
	td := c.td
	c.td = nil
	c.mu.Unlock()
	if td != nil {
		if err := closeTDClient(td, tdCloseTimeout); err != nil {
			c.logger.Warn("关闭 TDLib 失败: %v", err)
		}
	}
}

func closeTDClient(td tdAPI, timeout time.Duration) error {
	if td == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := tdCall(ctx, timeout, func(cc context.Context) (*tdclient.Ok, error) {
		return td.Close(cc)
	})
	return err
}

func (c *Client) client() tdAPI {
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
		return zero, fmt.Errorf("%w: %s", errTDTimeout, timeout)
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
	// proxy 非空时在首次网络活动前经 addProxy 启用（TDLib 不读 HTTP(S)_PROXY 环境变量）
	proxy *tdclient.AddProxyRequest
}

func (h *authHandler) Handle(td *tdclient.Client, state tdclient.AuthorizationState) error {
	switch state.AuthorizationStateConstructor() {
	case tdclient.ConstructorAuthorizationStateWaitTdlibParameters:
		// 顺序必须是先 SetTdlibParameters 再 addProxy：实测（TDLib 1.8.64）在参数设置前
		// 发出的 addProxy 永远得不到响应（请求方按 fallbackTimeout 挂死），而参数就绪后
		// addProxy 立即生效并让 TDLib 改经代理重连。TDLib 不读 HTTP(S)_PROXY 环境变量，
		// 不启用代理时被墙网络会永远卡在"正在连接"（issue #49）。
		// 启用失败直接中止本轮认证而不是静默直连——静默直连会把问题还原成无提示的卡死。
		if _, err := td.SetTdlibParameters(h.ctx, h.params); err != nil {
			return err
		}
		if h.proxy != nil {
			if _, err := td.AddProxy(h.ctx, h.proxy); err != nil {
				return fmt.Errorf("启用 Telegram 代理失败: %w", err)
			}
			h.c.logger.Info("已启用 Telegram 代理: %s", describeProxy(h.proxy))
		}
		return nil

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
	fd := int(os.Stdin.Fd())
	return readPasswordInput(os.Stdin, os.Stdout, term.IsTerminal(fd), func() ([]byte, error) {
		return term.ReadPassword(fd)
	})
}

// readPasswordInput 将终端与管道输入分开处理。真实终端使用 hiddenRead 关闭回显；
// 非终端输入保留按行读取，便于脚本和测试使用。
func readPasswordInput(
	in io.Reader, out io.Writer, terminal bool, hiddenRead func() ([]byte, error),
) (string, error) {
	if _, err := fmt.Fprint(out, "请输入两步验证密码: "); err != nil {
		return "", err
	}
	if terminal {
		password, err := hiddenRead()
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return string(password), nil
	}
	var password string
	if _, err := fmt.Fscanln(in, &password); err != nil {
		return "", err
	}
	return password, nil
}

// --- 监控目标 / 统计 / 会话 ---

// SetTargetChat 设置实时监控目标（0 表示停止监控）
func (c *Client) SetTargetChat(chatID int64) {
	c.monitorSwitchMu.Lock()
	defer c.monitorSwitchMu.Unlock()
	state := c.monitorSnapshot()
	state.chatID = chatID
	c.replaceMonitorLocked(state)
}

// TargetChat 返回当前监控目标聊天ID
func (c *Client) TargetChat() int64 { return c.monitorSnapshot().chatID }

// SetMonitorTask 设置当前监控任务ID并切换监控目标会话；taskID 为空字符串表示当前无关联任务。
//
// chatTitle 供路径模板的 {chat_title} 使用：由调用方（queue，已持有标题）传入而非在此查 TDLib，
// 否则同一个聊天的实时下载会因标题是否已缓存而落到不同目录。
func (c *Client) SetMonitorTask(taskID string, chatID int64, chatTitle string) {
	c.monitorSwitchMu.Lock()
	defer c.monitorSwitchMu.Unlock()
	c.replaceMonitorLocked(monitorState{chatID: chatID, taskID: taskID, chatTitle: chatTitle})
}

func (c *Client) stopMonitor() {
	c.monitorSwitchMu.Lock()
	c.replaceMonitorLocked(monitorState{})
	c.monitorSwitchMu.Unlock()
}

func newMonitorGeneration(state monitorState) *monitorGeneration {
	ctx, cancel := context.WithCancel(context.Background())
	return &monitorGeneration{state: state, ctx: ctx, cancel: cancel}
}

// replaceMonitorLocked 在锁内一次替换完整监控代，再取消并等待旧代。新消息要么归入旧代
// 并被等待，要么归入新代，不会看到空状态，也不会让旧代的 Wait 与新下载发生竞争。
// 调用方必须持有 monitorSwitchMu。
func (c *Client) replaceMonitorLocked(state monitorState) {
	var next *monitorGeneration
	if state.chatID != 0 {
		next = newMonitorGeneration(state)
	}
	c.monitorMu.Lock()
	previous := c.monitor
	c.monitor = next
	c.monitorMu.Unlock()
	if previous != nil {
		previous.cancel()
		previous.wg.Wait()
	}
}

func (c *Client) monitorSnapshot() monitorState {
	c.monitorMu.RLock()
	defer c.monitorMu.RUnlock()
	if c.monitor == nil {
		return monitorState{}
	}
	state := c.monitor.state
	return state
}

func (c *Client) beginMonitorDownload(
	chatID int64,
) (monitorState, context.Context, func(), bool) {
	c.monitorMu.Lock()
	defer c.monitorMu.Unlock()
	current := c.monitor
	if current == nil || current.state.chatID != chatID {
		return monitorState{}, nil, nil, false
	}
	current.wg.Add(1)
	return current.state, current.ctx, current.wg.Done, true
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

// SaveMetadata 返回是否为下载完成的文件写元数据 sidecar
func (c *Client) SaveMetadata() bool { return c.downloader.SaveMetadata() }

// SetSaveMetadata 切换元数据 sidecar（对后续下载生效），并写回 config.yaml
func (c *Client) SetSaveMetadata(v bool) error {
	c.downloader.SetSaveMetadata(v)
	c.credMu.Lock()
	c.config.Download.SaveMetadata = v
	c.credMu.Unlock()
	return c.SaveConfig()
}

// SetScanProgressFunc 设置历史扫描进度回调；须在 Connect/任务运行前注册
func (c *Client) SetScanProgressFunc(fn func(taskID string, scannedMessages, foundMedia, scanCursor int64)) {
	c.scanProgressFunc = fn
}

// SetRecordFunc 设置下载记录回调，用于持久化下载历史
func (c *Client) SetRecordFunc(fn func(context.Context, *downloader.RecordEvent)) {
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
	return c.config.HasAPICredentials()
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
	if err := removeSessionDatabase(c.config.Session.Dir, c.dbDir); err != nil {
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

	if err := removeSessionDatabase(c.config.Session.Dir, c.dbDir); err != nil {
		c.logger.Warn("清理 TDLib 会话目录失败: %v", err)
	}
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
func (c *Client) savedMessagesChat(ctx context.Context, td tdAPI) *ChatInfo {
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
func (c *Client) loadAllChats(ctx context.Context, td tdAPI, list tdclient.ChatList) {
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
	attachThumbnail(mi, m.Content)
	return mi
}

// attachThumbnail 提取缩略图信息：
//   - Minithumb 随消息免费返回（约 40x40 的 JPEG，几百字节），零下载成本，画廊拿它做即时占位图；
//   - ThumbFileID 指向真正的缩略图文件，需单独下载一次（小文件）。
//
// 照片没有 Thumbnail 字段，取 Sizes 里最小的一档当缩略图——总不能为了画廊网格
// 把每张 4000px 的原图都塞给浏览器。
func attachThumbnail(mi *downloader.MediaInfo, content tdclient.MessageContent) {
	file, mini := contentThumbs(content)
	if mini != nil {
		mi.Minithumb = mini.Data
	}
	if file != nil {
		mi.ThumbFileID = file.Id
		if file.Remote != nil {
			mi.ThumbUniqueID = file.Remote.UniqueId
		}
	}
}

// contentThumbs 按内容类型取出缩略图文件与 minithumbnail（均可为 nil）。
// 各 *Thumbs 取值器自行处理容器为 nil 的情况，因此这里不必逐个写 nil 判断。
func contentThumbs(content tdclient.MessageContent) (*tdclient.File, *tdclient.Minithumbnail) {
	switch c := content.(type) {
	case *tdclient.MessagePhoto:
		return photoThumbs(c.Photo)
	case *tdclient.MessageVideo:
		if c.Video == nil {
			return nil, nil
		}
		return thumbFile(c.Video.Thumbnail), c.Video.Minithumbnail
	case *tdclient.MessageDocument:
		if c.Document == nil {
			return nil, nil
		}
		return thumbFile(c.Document.Thumbnail), c.Document.Minithumbnail
	case *tdclient.MessageAnimation:
		if c.Animation == nil {
			return nil, nil
		}
		return thumbFile(c.Animation.Thumbnail), c.Animation.Minithumbnail
	case *tdclient.MessageAudio:
		if c.Audio == nil {
			return nil, nil
		}
		return thumbFile(c.Audio.AlbumCoverThumbnail), c.Audio.AlbumCoverMinithumbnail
	case *tdclient.MessageVideoNote:
		if c.VideoNote == nil {
			return nil, nil
		}
		return thumbFile(c.VideoNote.Thumbnail), c.VideoNote.Minithumbnail
	case *tdclient.MessageSticker:
		if c.Sticker == nil {
			return nil, nil
		}
		return thumbFile(c.Sticker.Thumbnail), nil // 贴纸没有 minithumbnail
	default:
		return nil, nil
	}
}

// photoThumbs 取照片的缩略图：照片没有 Thumbnail 字段，用 Sizes 里最小的一档
func photoThumbs(photo *tdclient.Photo) (*tdclient.File, *tdclient.Minithumbnail) {
	if photo == nil {
		return nil, nil
	}
	return smallestPhotoFile(photo), photo.Minithumbnail
}

// thumbFile 从 Thumbnail 里取出文件（容器或文件为 nil 时返回 nil）
func thumbFile(t *tdclient.Thumbnail) *tdclient.File {
	if t == nil {
		return nil
	}
	return t.File
}

// smallestPhotoFile 返回照片中面积最小的可用 size 对应的文件（照片没有独立的缩略图字段）
func smallestPhotoFile(photo *tdclient.Photo) *tdclient.File {
	if photo == nil {
		return nil
	}
	var best *tdclient.PhotoSize
	for _, s := range photo.Sizes {
		if s == nil || s.Photo == nil {
			continue
		}
		if best == nil || int64(s.Width)*int64(s.Height) < int64(best.Width)*int64(best.Height) {
			best = s
		}
	}
	if best == nil {
		return nil
	}
	return best.Photo
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
	// 各分支统一走"nil 安全取字段 → mediaFromFile"：容器为 nil 时字段取到零值，
	// mediaFromFile 见 file==nil 即返回 nil，因此这里不必逐个写 nil 判断。
	switch content := m.Content.(type) {
	case *tdclient.MessagePhoto:
		return mediaFromFile(m, largestPhotoFile(content.Photo), mediapkg.Photo,
			fmt.Sprintf("photo_%d_%d.jpg", m.ChatId, m.Id), "image/jpeg")
	case *tdclient.MessageDocument:
		f, name, mime := documentFields(content.Document)
		return mediaFromFile(m, f, mediapkg.Document, docName(name, m.Id), mime)
	case *tdclient.MessageVideo:
		f, name, mime := videoFields(content.Video)
		return mediaFromFile(m, f, mediapkg.Video, docName(name, m.Id), mime)
	case *tdclient.MessageAnimation:
		f, name, mime := animationFields(content.Animation)
		return mediaFromFile(m, f, mediapkg.Animation, docName(name, m.Id), mime)
	case *tdclient.MessageAudio:
		f, name, mime := audioFields(content.Audio)
		return mediaFromFile(m, f, mediapkg.Audio, docName(name, m.Id), mime)
	case *tdclient.MessageVoiceNote:
		f, mime := voiceFields(content.VoiceNote)
		return mediaFromFile(m, f, mediapkg.Voice,
			fmt.Sprintf("voice_%d_%d.ogg", m.ChatId, m.Id), mime)
	case *tdclient.MessageSticker:
		// 贴纸没有 FileName/MimeType，文件名与扩展名都得合成
		f, ext, mime := stickerFields(content.Sticker)
		return mediaFromFile(m, f, mediapkg.Sticker,
			fmt.Sprintf("sticker_%d_%d%s", m.ChatId, m.Id, ext), mime)
	case *tdclient.MessageVideoNote:
		// 圆形视频消息同样没有 FileName/MimeType，固定 mp4
		return mediaFromFile(m, videoNoteFile(content.VideoNote), mediapkg.VideoNote,
			fmt.Sprintf("video_note_%d_%d.mp4", m.ChatId, m.Id), "video/mp4")
	default:
		return nil
	}
}

// 以下 *Fields 取值器统一处理"容器可能为 nil"：返回零值即可，由 mediaFromFile 兜底成 nil。

func documentFields(d *tdclient.Document) (f *tdclient.File, name, mime string) {
	if d == nil {
		return nil, "", ""
	}
	return d.Document, d.FileName, d.MimeType
}

func videoFields(v *tdclient.Video) (f *tdclient.File, name, mime string) {
	if v == nil {
		return nil, "", ""
	}
	return v.Video, v.FileName, v.MimeType
}

func animationFields(a *tdclient.Animation) (f *tdclient.File, name, mime string) {
	if a == nil {
		return nil, "", ""
	}
	return a.Animation, a.FileName, a.MimeType
}

func audioFields(a *tdclient.Audio) (f *tdclient.File, name, mime string) {
	if a == nil {
		return nil, "", ""
	}
	return a.Audio, a.FileName, a.MimeType
}

func voiceFields(v *tdclient.VoiceNote) (f *tdclient.File, mime string) {
	if v == nil {
		return nil, ""
	}
	return v.Voice, v.MimeType
}

func videoNoteFile(v *tdclient.VideoNote) *tdclient.File {
	if v == nil {
		return nil
	}
	return v.Video
}

// stickerFields 取贴纸文件，并按 StickerFormat 决定扩展名与 MIME：
// webp = 静态贴纸，tgs = Lottie 动画（gzip 压缩的 JSON），webm = 视频贴纸。
func stickerFields(s *tdclient.Sticker) (f *tdclient.File, ext, mime string) {
	if s == nil {
		return nil, "", ""
	}
	switch s.Format.(type) {
	case *tdclient.StickerFormatTgs:
		return s.Sticker, extTgs, "application/x-tgsticker"
	case *tdclient.StickerFormatWebm:
		return s.Sticker, extWebm, "video/webm"
	default: // StickerFormatWebp 及未来可能新增的格式
		return s.Sticker, extWebp, "image/webp"
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
	attemptCtx, attempt := c.beginDownloadAttempt(ctx, media.TDFileID)
	defer c.finishDownloadAttempt(media.TDFileID, attempt)
	// 任务取消时 tdCall 会立即返回，但底层同步传输仍使用 background context；
	// 主动通知 TDLib 取消，避免它在后台继续占用网络与缓存文件。
	//
	// AfterFunc 在独立 goroutine 里执行。若不等它结束，DownloadFile 返回时取消请求可能
	// 尚未发出，monitorGeneration.wg.Wait() 便无法保证"停止监控后 TDLib 侧下载已取消"。
	// stopCancel 返回 false 表示回调已启动，此时必须等它跑完；cancelTDDownload 自带
	// metadataTimeout，不会无限期阻塞。
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		_ = c.cancelTDDownload(context.Background(), td, media.TDFileID)
	})
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()

	return c.retrier.Do(attemptCtx, func() error {
		c.logger.Info("下载文件: %s (大小: %d bytes)", media.FileName, media.FileSize)
		c.registerProgress(media)
		defer c.unregisterProgress(media.TDFileID)

		file, err := tdCall(attemptCtx, downloadTimeout(media.FileSize), func(cc context.Context) (*tdclient.File, error) {
			return td.DownloadFile(cc, &tdclient.DownloadFileRequest{
				FileId:      media.TDFileID,
				Priority:    downloadPriority,
				Offset:      0,
				Limit:       0,
				Synchronous: true,
			})
		})
		if err != nil {
			// 超时后主动取消底层下载，让 TDLib 释放该文件的传输资源
			if errors.Is(err, errTDTimeout) {
				_ = c.cancelTDDownload(context.Background(), td, media.TDFileID)
			}
			return fmt.Errorf("下载文件失败: %w", err)
		}
		if file.Local == nil || !file.Local.IsDownloadingCompleted || file.Local.Path == "" {
			return fmt.Errorf("%w: %s", errDownloadIncomplete, media.FileName)
		}
		return c.moveWithRetry(file.Local.Path, filePath)
	})
}

func (c *Client) beginDownloadAttempt(ctx context.Context, fileID int32) (context.Context, *downloadAttempt) {
	attemptCtx, cancel := context.WithCancel(ctx)
	attempt := &downloadAttempt{cancel: cancel}
	c.attemptMu.Lock()
	c.attempts[fileID] = attempt
	c.attemptMu.Unlock()
	return attemptCtx, attempt
}

func (c *Client) finishDownloadAttempt(fileID int32, attempt *downloadAttempt) {
	attempt.cancel()
	c.attemptMu.Lock()
	if c.attempts[fileID] == attempt {
		delete(c.attempts, fileID)
	}
	c.attemptMu.Unlock()
}

func (c *Client) cancelDownloadAttempt(fileID int32) {
	c.attemptMu.Lock()
	attempt := c.attempts[fileID]
	c.attemptMu.Unlock()
	if attempt != nil {
		attempt.cancel()
	}
}

func (c *Client) cancelAllDownloadAttempts() {
	c.attemptMu.Lock()
	attempts := make([]*downloadAttempt, 0, len(c.attempts))
	for _, attempt := range c.attempts {
		attempts = append(attempts, attempt)
	}
	c.attemptMu.Unlock()
	for _, attempt := range attempts {
		attempt.cancel()
	}
}

// DownloadThumbFile 下载一个缩略图文件到 destPath。
//
// 与主文件下载刻意分开：缩略图不进进度表（否则 UI 上每个媒体会冒出两条进度）、
// 不走 retrier（缩略图失败无关紧要，重试的代价高过收益）、失败只返回错误由调用方吞掉。
func (c *Client) DownloadThumbFile(ctx context.Context, fileID int32, destPath string) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	file, err := tdCall(ctx, thumbDownloadTimeout, func(cc context.Context) (*tdclient.File, error) {
		return td.DownloadFile(cc, &tdclient.DownloadFileRequest{
			FileId:      fileID,
			Priority:    thumbDownloadPriority,
			Offset:      0,
			Limit:       0,
			Synchronous: true,
		})
	})
	if err != nil {
		return fmt.Errorf("下载缩略图失败: %w", err)
	}
	if file.Local == nil || !file.Local.IsDownloadingCompleted || file.Local.Path == "" {
		return errDownloadIncomplete
	}
	return c.moveWithRetry(file.Local.Path, destPath)
}

func (c *Client) pauseDownloadFile(ctx context.Context, media *downloader.MediaInfo) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	// 先终止 retrier，确保 CancelDownloadFile 导致当前请求返回“未完成”时不会自动重启。
	c.cancelDownloadAttempt(media.TDFileID)
	return c.cancelTDDownload(ctx, td, media.TDFileID)
}

func (c *Client) cancelTDDownload(ctx context.Context, td tdAPI, fileID int32) error {
	_, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Ok, error) {
		return td.CancelDownloadFile(cc, &tdclient.CancelDownloadFileRequest{
			FileId:        fileID,
			OnlyIfPending: false,
		})
	})
	return err
}

// moveWithRetry 将 TDLib 缓存文件移动到目标路径。
//
// 按失败原因分流，而不是对任何错误都盲目重试 5 次：
//   - EXDEV（跨文件系统）：rename 永远不可能成功，直接复制，重试纯属浪费；
//   - ENOSPC（磁盘已满）：重试同样注定失败。立即返回，避免每个文件白等 2.5 秒——
//     一个几千文件的任务在磁盘满时会空转数小时；
//   - 其余错误（如 Windows 上的文件占用、瞬时锁）：短暂重试仍有意义。
func (c *Client) moveWithRetry(src, dst string) error {
	var err error
	for attempt := 0; attempt < MaxRenameRetries; attempt++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		if errors.Is(err, syscall.EXDEV) {
			break // 跨设备：直接走复制回退
		}
		if errors.Is(err, syscall.ENOSPC) {
			return fmt.Errorf("磁盘空间不足，移动文件失败: %w", err)
		}
		c.logger.Warn("移动文件失败 (尝试 %d): %v", attempt+1, err)
		time.Sleep(RenameSleepDuration)
	}

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

	tmp, err := os.CreateTemp(filepath.Dir(filepath.Clean(dst)), ".td-copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	buf := make([]byte, copyBufferSize)
	if _, err = io.CopyBuffer(tmp, in, buf); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err = os.Rename(tmpName, filepath.Clean(dst)); err != nil {
		_ = os.Remove(tmpName)
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

// historyCountFilters 将媒体类型映射到服务端过滤器，用于枚举与计数。
//
// 只覆盖 media.HasServerFilter 为真的类型——贴纸没有对应的 TDLib 过滤器，故不在此表中。
// TestServerFilterCoverage 强制本表与 media 包的类型集保持同步。
var historyCountFilters = map[string]tdclient.SearchMessagesFilter{
	mediapkg.Photo:     &tdclient.SearchMessagesFilterPhoto{},
	mediapkg.Video:     &tdclient.SearchMessagesFilterVideo{},
	mediapkg.Document:  &tdclient.SearchMessagesFilterDocument{},
	mediapkg.Audio:     &tdclient.SearchMessagesFilterAudio{},
	mediapkg.Voice:     &tdclient.SearchMessagesFilterVoiceNote{},
	mediapkg.Animation: &tdclient.SearchMessagesFilterAnimation{},
	mediapkg.VideoNote: &tdclient.SearchMessagesFilterVideoNote{},
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
func (c *Client) resolveMessageLink(ctx context.Context, td tdAPI, url string) (ResolvedTarget, error) {
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

// ChatTitle 查询聊天标题（失败返回空串）。供 CLI 填充 HistorySpec.ChatTitle：
// 少了它，同一个聊天用 CLI 下和用 Web 下会因 {chat_title} 展开不同而落到两个目录。
func (c *Client) ChatTitle(ctx context.Context, chatID int64) string {
	td := c.client()
	if td == nil {
		return ""
	}
	chat, err := tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Chat, error) {
		return td.GetChat(cc, &tdclient.GetChatRequest{ChatId: chatID})
	})
	if err != nil {
		return ""
	}
	return chat.Title
}

// resolvePublicChat 按公开用户名解析聊天
func (c *Client) resolvePublicChat(ctx context.Context, td tdAPI, username string) (ResolvedTarget, error) {
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
//
// 选中的类型里只要有一个无服务端计数能力（贴纸），总数就返回 0（= 未知）：
// 拿其余类型的和当分母，分子却含贴纸，进度条会冲破 100%。宁可没有分母，也不要错的分母。
func (c *Client) CountHistoryMedia(ctx context.Context, chatID int64, mediaTypes []string) (int64, error) {
	td := c.client()
	if td == nil {
		return 0, errors.New("TDLib 未连接")
	}
	types := mediaTypes
	if len(types) == 0 {
		types = mediapkg.DefaultTypes
	}
	for _, t := range types {
		if !mediapkg.HasServerFilter(t) {
			c.logger.Info("类型 %s 无服务端计数能力，本任务总数按未知处理", t)
			return 0, nil
		}
	}

	selected := make([]tdclient.SearchMessagesFilter, 0, len(types))
	for _, t := range types {
		if f, ok := historyCountFilters[t]; ok {
			selected = append(selected, f)
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
// 单消息任务（spec.MessageID != 0）只下载指定消息；恢复任务先补下被重启清扫的中断行。
//
// 返回值 HistoryResult 汇总本次运行的单文件失败数。error 只表示"任务级"失败（扫描出错、
// 聊天不可访问、被取消）——单个文件下载失败不会中止任务，但会计入 result.Failed，
// 由调用方决定任务终态。
func (c *Client) DownloadHistoryMedia(
	ctx context.Context, spec *downloader.HistorySpec,
) (*downloader.HistoryResult, error) {
	td := c.client()
	if td == nil {
		return &downloader.HistoryResult{}, errors.New("TDLib 未连接")
	}
	if spec.FromMessageID > 0 {
		c.logger.Info("继续下载聊天 %d 的历史媒体文件（游标 %d）", spec.ChatID, spec.FromMessageID)
	} else {
		c.logger.Info("开始下载聊天 %d 的历史媒体文件", spec.ChatID)
	}

	closeChat, err := c.openChatForHistory(ctx, td, spec.ChatID)
	if err != nil {
		return &downloader.HistoryResult{}, err
	}
	if closeChat != nil {
		defer closeChat()
	}

	pipe := c.newDownloadPipeline(ctx)
	defer pipe.wait()

	scan, runErr := c.runHistorySpec(ctx, td, spec, pipe)
	pipe.wait()
	return &downloader.HistoryResult{
		Failed:       pipe.failed.Load(),
		MaxMessageID: scan.maxMessageID,
	}, runErr
}

// runHistorySpec 按 spec 的形态选择执行路径，把发现的媒体投入下载流水线。
// 返回的 error 只表示任务级失败；单文件失败由 pipeline 计数。
func (c *Client) runHistorySpec(
	ctx context.Context, td tdAPI, spec *downloader.HistorySpec, pipe *downloadPipeline,
) (scanOutcome, error) {
	// 单消息任务（t.me 消息链接）：只下载指定消息，不扫描历史
	if spec.MessageID != 0 {
		return scanOutcome{}, c.downloadSingleHistoryMessage(ctx, td, spec, pipe.dispatch)
	}

	// 恢复/重试任务先补下失败的行：这些消息可能比游标更新，续扫不会再经过
	if len(spec.RetryMessageIDs) > 0 {
		if err := c.retryInterruptedMessages(ctx, td, spec, pipe.dispatch); err != nil {
			return scanOutcome{}, err
		}
	}

	// RetryOnly：历史已完整扫过，本次只补失败的文件——重扫一遍不会有任何新发现
	if spec.RetryOnly {
		return scanOutcome{}, nil
	}

	scan, scanErr := c.scanHistoryPages(ctx, td, spec, pipe.dispatch)
	if scanErr != nil {
		return scan, scanErr
	}
	c.logger.Info("历史扫描完成: 共扫描 %d 条消息，发现 %d 个媒体", scan.scannedMessages, scan.foundMedia)
	return scan, nil
}

// downloadPipeline 是扫描与下载之间的流水线：扫描一发现媒体就分发下载，
// sem 限制扫描最多领先下载 partitionSize 个在途媒体（内存与队列长度上界）。
type downloadPipeline struct {
	wg         sync.WaitGroup
	failed     atomic.Int64
	sem        chan struct{}
	dispatchFn func(*downloader.MediaInfo) error
}

// newDownloadPipeline 建立下载流水线，在途上限取自 download.partition_size
func (c *Client) newDownloadPipeline(ctx context.Context) *downloadPipeline {
	partitionSize := c.config.Download.PartitionSize
	if partitionSize <= 0 {
		partitionSize = config.DefaultPartitionSize
	}
	p := &downloadPipeline{sem: make(chan struct{}, partitionSize)}
	p.dispatchFn = func(mi *downloader.MediaInfo) error {
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-p.sem }()
			if err := c.downloader.DownloadMedia(ctx, mi); err != nil {
				// 单个文件失败不中止任务（一个文件挂了不该让其余几千个停下），但必须计数：
				// 此前这里只打一行日志就把错误丢掉，导致"全部文件下载失败"的任务依然被判定
				// 为已完成——用户看到绿色的"已完成"，而一个文件都没下来。
				// 任务取消导致的失败不计入：那是用户的意图，不是故障。
				if ctx.Err() == nil {
					p.failed.Add(1)
				}
				c.logger.Error("下载媒体文件失败: %v", err)
			}
		}()
		return nil
	}
	return p
}

func (p *downloadPipeline) dispatch(mi *downloader.MediaInfo) error { return p.dispatchFn(mi) }

// wait 等待所有在途下载退出（可重复调用）
func (p *downloadPipeline) wait() { p.wg.Wait() }

// openChatForHistory 校验聊天可访问并打开聊天，促使 TDLib 主动从服务器同步历史。
// 返回的 closeFn（可为 nil）应在拉取结束后调用以释放 TDLib 资源。
func (c *Client) openChatForHistory(ctx context.Context, td tdAPI, chatID int64) (closeFn func(), err error) {
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

// scanHistoryPages 向更旧方向扫描聊天历史，按任务过滤器筛选并分发下载。
//
// 优先走服务端枚举：每种媒体类型各跑一条 SearchChatMessages 流水线（该 API 一次只接受一个
// 过滤器），服务端只回匹配的媒体消息，且结束信号由 NextFromMessageId == 0 明确给出。
// 只要所选类型含贴纸等没有专用过滤器的类型，就改用一条 SearchMessagesFilterEmpty 流水线
// 枚举所有消息并在本地筛选。它会多传回纯文本，但仍以 NextFromMessageId == 0 明确结束，
// 不会像 GetChatHistory 的重复空页那样把“服务端仍在回填”误判成扫描完成。
//
// 游标（scan_cursor）只在单条流水线时持久化：一个整数无法表达 N 条流水线各自的位置。
// 多流水线任务重启后从最新重扫，但扫的只是媒体消息，且已下载的文件由去重/跳过挡掉。
func (c *Client) scanHistoryPages(
	ctx context.Context, td tdAPI, spec *downloader.HistorySpec,
	dispatch func(*downloader.MediaInfo) error,
) (scanOutcome, error) {
	batchSize := c.config.Download.BatchSize
	if batchSize <= 0 || batchSize > DefaultMessageLimit {
		batchSize = DefaultMessageLimit
	}
	limit := int32(batchSize) // 已上界钳制到 DefaultMessageLimit(100)，不会溢出

	types := effectiveTypes(spec.Filters.MediaTypes)
	filters, ok := searchFiltersFor(types)
	if !ok {
		start := c.scanStartID(ctx, td, spec, false)
		c.logger.Info("所选类型需要完整历史，使用单条无过滤搜索流水线")
		return c.scanBySearch(
			ctx, td, spec, &tdclient.SearchMessagesFilterEmpty{}, start, limit, dispatch, scanOutcome{}, true,
		)
	}

	multi := len(filters) > 1
	start := c.scanStartID(ctx, td, spec, multi)
	c.logger.Info("按类型 %v 走服务端枚举（%d 条流水线）", types, len(filters))

	var res scanOutcome
	for i, f := range filters {
		out, err := c.scanBySearch(ctx, td, spec, f, start, limit, dispatch, res, !multi)
		if err != nil {
			return out, fmt.Errorf("扫描 %s 失败: %w", types[i], err)
		}
		res = out
	}
	return res, nil
}

// retryInterruptedMessages 逐条重取并补下恢复任务的中断消息。
// 仍可下载的消息会先分发；已删除、无媒体或被过滤器排除的消息汇总为任务级错误，
// 防止 RetryOnly 把没有实际恢复的失败记录误报为完成。
func (c *Client) retryInterruptedMessages(
	ctx context.Context, td tdAPI, spec *downloader.HistorySpec,
	dispatch func(*downloader.MediaInfo) error,
) error {
	var batch []*downloader.MediaInfo
	var unresolved []error
	for _, msgID := range spec.RetryMessageIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := c.getMessage(ctx, td, spec.ChatID, msgID)
		if err != nil {
			unresolved = append(unresolved, fmt.Errorf("补下消息 %d 获取失败: %w", msgID, err))
			continue
		}
		media := c.extractMediaInfo(msg)
		if media == nil {
			unresolved = append(unresolved, fmt.Errorf("补下消息 %d 不包含可下载的媒体", msgID))
			continue
		}
		if !spec.Filters.Match(media) {
			unresolved = append(unresolved, fmt.Errorf("补下消息 %d 的媒体被任务过滤器排除", msgID))
			continue
		}
		media.TaskID = spec.TaskID
		media.ChatTitle = spec.ChatTitle
		batch = append(batch, media)
	}
	c.downloader.PlanBatch(batch)
	for _, m := range batch {
		if err := dispatch(m); err != nil {
			unresolved = append(unresolved, err)
			break
		}
	}
	return errors.Join(unresolved...)
}

// getMessage 取单条消息，经 retrier 重试（限流/网络抖动不应让补下或单消息任务直接失败）
func (c *Client) getMessage(ctx context.Context, td tdAPI, chatID, msgID int64) (*tdclient.Message, error) {
	var msg *tdclient.Message
	err := c.retrier.Do(ctx, func() error {
		var err error
		msg, err = tdCall(ctx, metadataTimeout, func(cc context.Context) (*tdclient.Message, error) {
			return td.GetMessage(cc, &tdclient.GetMessageRequest{ChatId: chatID, MessageId: msgID})
		})
		return err
	})
	return msg, err
}

// downloadSingleHistoryMessage 下载单条消息的媒体（t.me 消息链接任务）
func (c *Client) downloadSingleHistoryMessage(
	ctx context.Context, td tdAPI, spec *downloader.HistorySpec,
	dispatch func(*downloader.MediaInfo) error,
) error {
	msg, err := c.getMessage(ctx, td, spec.ChatID, spec.MessageID)
	if err != nil {
		return fmt.Errorf("获取消息 %d 失败: %w", spec.MessageID, err)
	}
	media := c.extractMediaInfo(msg)
	if media == nil {
		return fmt.Errorf("消息 %d 不包含可下载的媒体", spec.MessageID)
	}
	if !spec.Filters.Match(media) {
		return fmt.Errorf("消息 %d 的媒体被任务过滤器排除", spec.MessageID)
	}
	media.TaskID = spec.TaskID
	media.ChatTitle = spec.ChatTitle
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

// extractBatchMedia 从一页历史消息中提取媒体信息、按任务过滤器筛选并打上任务ID与聊天标题；
// 同时返回整页是否已早于 DateFrom（可提前停止翻页：历史页按新到旧返回，
// 整页更旧则更早的页必然全部越界）
func (c *Client) extractBatchMedia(
	msgs []*tdclient.Message, spec *downloader.HistorySpec,
) (media []*downloader.MediaInfo, pastDateFrom bool) {
	filters := spec.Filters
	pastDateFrom = len(msgs) > 0 && filters.DateFrom != 0
	for _, m := range msgs {
		if int64(m.Date) >= filters.DateFrom {
			pastDateFrom = false
		}
		if mi := c.extractMediaInfo(m); mi != nil && filters.Match(mi) {
			mi.TaskID = spec.TaskID
			mi.ChatTitle = spec.ChatTitle
			media = append(media, mi)
		}
	}
	return media, pastDateFrom
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
	monitor, monitorCtx, monitorDone, ok := c.beginMonitorDownload(m.ChatId)
	if !ok {
		return
	}
	media := c.extractMediaInfo(m)
	if media == nil {
		monitorDone()
		c.logger.Info("📝 目标聊天新消息（无媒体）: %s", messagePreview(m))
		return
	}
	media.TaskID = monitor.taskID
	media.ChatTitle = monitor.chatTitle
	c.logger.Info("🎬 检测到目标聊天新媒体: %s", media.FileName)
	go func() {
		defer monitorDone()
		c.downloader.DownloadSingle(monitorCtx, media)
	}()
}

// onConnectionState 输出连接状态变化
func (c *Client) onConnectionState(state tdclient.ConnectionState) {
	if state == nil {
		return
	}
	switch state.(type) {
	case *tdclient.ConnectionStateReady:
		c.logger.Debug("TDLib 连接就绪")
		c.connState.Store(ConnectionReady)
	case *tdclient.ConnectionStateConnecting, *tdclient.ConnectionStateConnectingToProxy:
		c.logger.Debug("TDLib 正在连接...")
		c.connState.Store(ConnectionConnecting)
	case *tdclient.ConnectionStateUpdating:
		c.connState.Store(ConnectionUpdating)
	case *tdclient.ConnectionStateWaitingForNetwork:
		c.logger.Warn("TDLib 等待网络...")
		c.connState.Store(ConnectionWaitingNetwork)
	}
}

// 网络连接状态。此前 TDLib 的连接状态只写日志，界面无从得知"下载停滞是因为断网"，
// 用户只能看着进度条不动干着急。
const (
	ConnectionReady          = "ready"
	ConnectionConnecting     = "connecting"
	ConnectionUpdating       = "updating"
	ConnectionWaitingNetwork = "waiting_network"
)

// ConnectionState 返回当前的 TDLib 网络连接状态（未连接时为空串）
func (c *Client) ConnectionState() string {
	s, _ := c.connState.Load().(string)
	return s
}

// connectionStateLabels 把内部连接状态码映射为日志可读标签
var connectionStateLabels = map[string]string{
	ConnectionReady:          "已连接",
	ConnectionConnecting:     "连接中",
	ConnectionUpdating:       "更新中",
	ConnectionWaitingNetwork: "等待网络",
}

// connectHintDelays 可在测试中缩短的连接停滞提示节奏（首条提示、重复间隔）
var (
	connectHintDelay  = 30 * time.Second
	connectHintRepeat = 5 * time.Minute
)

// runConnectHint 在 Connect 阻塞于认证/连接期间监测停滞：超过 connectHintDelay 仍未
// 就绪就输出排查提示，之后按 connectHintRepeat 重复。此前网络不通时只会无限期静默
// 卡在"正在连接 Telegram"，用户无从判断是程序问题还是网络问题（issue #49）。
// done 由调用方在 Connect 返回时关闭；已进入 Ready 状态则立即退出。
func (c *Client) runConnectHint(withProxy bool, done <-chan struct{}) {
	c.runConnectHintWith(connectHintDelay, connectHintRepeat, withProxy, done)
}

// runConnectHintWith 是 runConnectHint 的可注入节奏版本（测试用短间隔驱动同一逻辑）。
func (c *Client) runConnectHintWith(delay, repeat time.Duration, withProxy bool, done <-chan struct{}) {
	start := time.Now()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-timer.C:
		}
		if c.ConnectionState() == ConnectionReady {
			return
		}
		elapsed := time.Since(start).Round(time.Second)
		stateLabel := connectionStateLabels[c.ConnectionState()]
		if stateLabel == "" {
			stateLabel = "未知"
		}
		if withProxy {
			c.logger.Warn("已等待 %v 仍未连上 Telegram（状态: %s）。请检查代理服务器是否可用、代理地址与端口是否正确", elapsed, stateLabel)
		} else {
			c.logger.Warn(
				"已等待 %v 仍未连上 Telegram（状态: %s）。若本机网络无法直连 Telegram，"+
					"请配置代理：config.yaml 的 telegram.proxy 或环境变量 TG_PROXY / ALL_PROXY / HTTPS_PROXY / HTTP_PROXY（支持 socks5:// 与 http://），修改后重启生效",
				elapsed, stateLabel)
		}
		timer.Reset(repeat)
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
