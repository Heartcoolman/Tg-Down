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
	emptyHistoryRetries = 5
	// emptyHistorySleep is the wait between consecutive empty history pages.
	emptyHistorySleep = 1 * time.Second

	mediaTypePhoto     = "photo"
	mediaTypeDocument  = "document"
	mediaTypeVideo     = "video"
	mediaTypeAnimation = "animation"
	mediaTypeAudio     = "audio"
	mediaTypeVoice     = "voice"

	copyBufferSize = 1 << 20 // 1MB copy buffer for cross-device fallback

	// appVersion 上报给 TDLib 的设备/应用版本
	appVersion = "1.0"
	// dbDirPerm 是 TDLib 数据库目录权限
	dbDirPerm = 0o700
	// tdNotFoundCode 是 TDLib 列表耗尽时返回的错误码
	tdNotFoundCode = 404
)

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
	lastMessageID int64        // 手动轮询游标

	mu sync.Mutex
	td *tdclient.Client // Connect 后才有值

	credMu sync.Mutex // 保护 config.API 凭据（Web 端可动态注入）

	trackMu   sync.Mutex
	fileTrack map[int32]*fileProgress // TDLib file id -> 进度信息（用于日志）
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
	c.downloader.SetClassifyByType(!cfg.Download.DisableClassifyByType)
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

// SetRecordFunc 设置下载记录回调，用于持久化下载历史
func (c *Client) SetRecordFunc(fn func(context.Context, downloader.RecordEvent)) {
	c.downloader.SetRecordFunc(fn)
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

// --- 聊天枚举 ---

// GetChats 获取聊天列表（主文件夹 + 归档）
func (c *Client) GetChats(ctx context.Context) ([]ChatInfo, error) {
	td := c.client()
	if td == nil {
		return nil, errors.New("TDLib 未连接")
	}
	mctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	seen := make(map[int64]bool)
	var order []int64
	for _, list := range []tdclient.ChatList{&tdclient.ChatListMain{}, &tdclient.ChatListArchive{}} {
		c.loadAllChats(mctx, td, list)
		chats, err := td.GetChats(mctx, &tdclient.GetChatsRequest{ChatList: list, Limit: maxChatLimit})
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

	result := make([]ChatInfo, 0, len(order))
	for _, id := range order {
		chat, err := td.GetChat(mctx, &tdclient.GetChatRequest{ChatId: id})
		if err != nil {
			continue
		}
		if info := chatInfoOf(chat); info != nil {
			result = append(result, *info)
		}
	}
	return result, nil
}

// loadAllChats 反复调用 LoadChats 把指定列表全部载入本地缓存，直到 404（无更多）
func (c *Client) loadAllChats(ctx context.Context, td *tdclient.Client, list tdclient.ChatList) {
	for {
		_, err := td.LoadChats(ctx, &tdclient.LoadChatsRequest{ChatList: list, Limit: chatLoadBatch})
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
	return &downloader.MediaInfo{
		MessageID: m.Id,
		TDFileID:  f.Id,
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

		file, err := td.DownloadFile(ctx, &tdclient.DownloadFileRequest{
			FileId:      media.TDFileID,
			Priority:    downloadPriority,
			Offset:      0,
			Limit:       0,
			Synchronous: true,
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

// copyFile 复制文件内容到目标路径
func copyFile(src, dst string) error {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(filepath.Clean(dst))
	if err != nil {
		return err
	}

	buf := make([]byte, copyBufferSize)
	if _, err = io.CopyBuffer(out, in, buf); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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

// DownloadHistoryMedia 下载聊天历史中的全部媒体
func (c *Client) DownloadHistoryMedia(ctx context.Context, chatID int64, taskID string) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	c.logger.Info("开始下载聊天 %d 的历史媒体文件", chatID)

	if _, err := td.GetChat(ctx, &tdclient.GetChatRequest{ChatId: chatID}); err != nil {
		return fmt.Errorf("无法访问聊天 %d: %w", chatID, err)
	}

	// 打开聊天，促使 TDLib 主动从服务器同步历史；冷缓存时首批 GetChatHistory 常为空，
	// 否则可能在历史尚未拉取就误判"已完成"。结束时关闭以释放 TDLib 资源。
	if _, err := td.OpenChat(ctx, &tdclient.OpenChatRequest{ChatId: chatID}); err != nil {
		c.logger.Warn("打开聊天失败（继续尝试拉取历史）: %v", err)
	} else {
		defer func() { _, _ = td.CloseChat(context.Background(), &tdclient.CloseChatRequest{ChatId: chatID}) }()
	}

	batchSize := c.config.Download.BatchSize
	if batchSize <= 0 || batchSize > DefaultMessageLimit {
		batchSize = DefaultMessageLimit
	}
	limit := int32(batchSize) // 已上界钳制到 DefaultMessageLimit(100)，不会溢出

	var fromMsgID int64
	totalMedia := 0
	emptyStreak := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pageMsgs, err := fetchHistoryPage(ctx, td, chatID, fromMsgID, limit)
		if err != nil {
			return err
		}

		if len(pageMsgs) == 0 {
			emptyStreak++
			stop, err := awaitNextHistoryPage(ctx, emptyStreak)
			if err != nil {
				return err
			}
			if stop {
				break
			}
			continue
		}
		emptyStreak = 0

		media, lastMsgID := c.extractBatchMedia(pageMsgs, taskID)
		fromMsgID = lastMsgID // 推进到本页最旧消息

		if len(media) > 0 {
			wg := c.downloader.DownloadBatch(ctx, media)
			wg.Wait()
			totalMedia += len(media)
			c.logger.Info("已处理 %d 个媒体文件", totalMedia)
		}
	}

	c.logger.Info("历史媒体文件下载完成，总计处理 %d 个文件", totalMedia)
	c.downloader.PrintStats()
	return nil
}

// fetchHistoryPage 拉取一页历史消息，并剔除 Offset:0 时 TDLib 附带返回的
// FromMessageId 边界消息本身（非首次请求时），避免重复处理及"仅剩边界消息"导致的死循环
func fetchHistoryPage(ctx context.Context, td *tdclient.Client, chatID, fromMsgID int64, limit int32) ([]*tdclient.Message, error) {
	mctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	msgs, err := td.GetChatHistory(mctx, &tdclient.GetChatHistoryRequest{
		ChatId:        chatID,
		FromMessageId: fromMsgID,
		Offset:        0,
		Limit:         limit,
		OnlyLocal:     false,
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

// extractBatchMedia 从一页历史消息中提取媒体信息并打上任务ID，
// 同时返回本页最旧消息ID供调用方推进下一页起点
func (c *Client) extractBatchMedia(msgs []*tdclient.Message, taskID string) (media []*downloader.MediaInfo, lastMsgID int64) {
	for _, m := range msgs {
		if mi := c.extractMediaInfo(m); mi != nil {
			mi.TaskID = taskID
			media = append(media, mi)
		}
		lastMsgID = m.Id
	}
	return media, lastMsgID
}

// awaitNextHistoryPage 处理获取到空历史页时的退避逻辑：
// 连续空页达到阈值则停止轮询（stop=true）；否则等待后允许继续下一页。
func awaitNextHistoryPage(ctx context.Context, emptyStreak int) (stop bool, err error) {
	if emptyStreak >= emptyHistoryRetries {
		return true, nil
	}
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-time.After(emptyHistorySleep):
	}
	return false, nil
}

// ManualCheckNewMessages 手动检查新消息（CLI 监控模式）
func (c *Client) ManualCheckNewMessages(ctx context.Context, chatID int64) error {
	c.logger.Info("开始手动检查聊天 %d 的新消息", chatID)

	latestID := c.getLastMessageID(ctx, chatID)
	if latestID == 0 {
		return errors.New("无法获取最新消息ID")
	}
	c.logger.Info("当前最新消息ID: %d", latestID)

	if c.lastMessageID == 0 {
		c.lastMessageID = latestID
		c.logger.Info("初始化 lastMessageID 为: %d", c.lastMessageID)
		return nil
	}
	if latestID <= c.lastMessageID {
		c.logger.Info("没有新消息")
		return nil
	}

	c.logger.Info("发现新消息！从 %d 到 %d", c.lastMessageID, latestID)
	if err := c.checkForNewMessages(ctx, chatID, c.lastMessageID); err != nil {
		return fmt.Errorf("检查新消息失败: %w", err)
	}
	c.lastMessageID = latestID
	return nil
}

// getLastMessageID 获取聊天最新消息ID
func (c *Client) getLastMessageID(ctx context.Context, chatID int64) int64 {
	td := c.client()
	if td == nil {
		return 0
	}
	mctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	msgs, err := td.GetChatHistory(mctx, &tdclient.GetChatHistoryRequest{ChatId: chatID, Limit: 1, OnlyLocal: false})
	if err != nil {
		c.logger.Error("获取最新消息ID失败: %v", err)
		return 0
	}
	if len(msgs.Messages) > 0 {
		return msgs.Messages[0].Id
	}
	return 0
}

// checkForNewMessages 拉取近期消息并下载其中超过游标的新媒体
func (c *Client) checkForNewMessages(ctx context.Context, chatID, lastMessageID int64) error {
	td := c.client()
	if td == nil {
		return errors.New("TDLib 未连接")
	}
	mctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	msgs, err := td.GetChatHistory(mctx, &tdclient.GetChatHistoryRequest{
		ChatId: chatID, Limit: DefaultMessageLimit, OnlyLocal: false,
	})
	if err != nil {
		return fmt.Errorf("获取消息历史失败: %w", err)
	}

	newCount, mediaCount := 0, 0
	for _, m := range msgs.Messages {
		if m.Id <= lastMessageID {
			continue
		}
		newCount++
		c.logger.Info("发现新消息 ID: %d, 内容: %s", m.Id, messagePreview(m))
		if media := c.extractMediaInfo(m); media != nil {
			mediaCount++
			c.logger.Info("新消息包含媒体，开始下载: %s", media.FileName)
			c.downloader.DownloadSingle(ctx, media)
		}
	}

	if newCount > 0 {
		c.logger.Info("本次检查发现 %d 条新消息，其中 %d 条包含媒体", newCount, mediaCount)
	} else {
		c.logger.Debug("本次检查未发现新消息")
	}
	return nil
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
	}
}

// onUpdateFile 按字节间隔输出下载进度日志
func (c *Client) onUpdateFile(f *tdclient.File) {
	if f == nil || f.Local == nil {
		return
	}
	c.trackMu.Lock()
	fp := c.fileTrack[f.Id]
	var logLine string
	if fp != nil {
		done := f.Local.DownloadedSize
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
