// Package telegram provides Telegram client functionality for Tg-Down application.
// It handles authentication, chat management, and media downloading operations.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/downloader/chunked"
	"tg-down/internal/logger"
	"tg-down/internal/middleware/floodwait"
	"tg-down/internal/middleware/ratelimit"
	"tg-down/internal/retry"
	"tg-down/internal/session"
)

// Client Telegram客户端包装器
type Client struct {
	Client          *telegram.Client
	API             *tg.Client
	config          *config.Config
	logger          *logger.Logger
	downloader      *downloader.Downloader
	chunkedDownloader *chunked.ChunkDownloader
	sessionMgr      *session.Manager
	targetChatID    int64 // 目标聊天ID，用于实时监控
	lastMessageID   int   // 最后处理的消息ID
	floodWaiter     *floodwait.Waiter
	rateLimiter     *ratelimit.Limiter
	retrier         *retry.Retrier
}

// New 创建新的Telegram客户端
func New(cfg *config.Config, logger *logger.Logger) *Client {
	sessionMgr := session.New(cfg.Session.Dir, logger)

	// 创建中间件
	floodWaiter := floodwait.New(logger)

	rateLimiter := ratelimit.New(
		rate.Limit(cfg.RateLimit.RequestsPerSecond),
		cfg.RateLimit.BurstSize,
		logger,
	)

	// 创建带中间件的客户端
	tgClient := sessionMgr.CreateClientWithMiddleware(
		cfg.API.ID,
		cfg.API.Hash,
		cfg.API.Phone,
		floodWaiter,
		rateLimiter,
	)

	if tgClient == nil {
		logger.Error("无法创建Telegram客户端")
		return nil
	}

	// 创建重试器
	retrier := retry.NewDefault(logger).
		WithMaxRetries(cfg.Retry.MaxRetries).
		WithBaseDelay(time.Duration(cfg.Retry.BaseDelay) * time.Second).
		WithMaxDelay(time.Duration(cfg.Retry.MaxDelay) * time.Second)

	// 创建分块下载器
	chunkedDownloader := chunked.New(logger).
		WithChunkSize(cfg.Download.ChunkSize * 1024). // 转换为字节
		WithMaxWorkers(cfg.Download.MaxWorkers)

	c := &Client{
		Client:            tgClient,
		API:               tgClient.API(),
		config:            cfg,
		logger:            logger,
		sessionMgr:        sessionMgr,
		floodWaiter:       floodWaiter,
		rateLimiter:       rateLimiter,
		retrier:           retrier,
		chunkedDownloader: chunkedDownloader,
	}

	// 创建下载器
	c.downloader = downloader.New(tgClient, cfg.Download.Path, cfg.Download.MaxConcurrent, logger)
	c.downloader.SetDownloadFunc(c.DownloadFile)

	// 检查是否有现有会话
	if sessionMgr.HasValidSession(cfg.API.Phone) {
		logger.Info("发现现有会话文件，将尝试自动登录")
	} else {
		logger.Info("未发现会话文件，需要进行首次登录")
	}

	logger.Info("已创建优化的Telegram客户端 (分块下载: %v, 速率限制: %.1f req/s, 重试: %d次)",
		cfg.Download.UseChunked,
		cfg.RateLimit.RequestsPerSecond,
		cfg.Retry.MaxRetries,
	)

	return c
}

// NewWithUpdates 创建带Updates处理器的Telegram客户端
func NewWithUpdates(cfg *config.Config, logger *logger.Logger, chatID int64) *Client {
	sessionMgr := session.New(cfg.Session.Dir, logger)

	// 创建中间件
	floodWaiter := floodwait.New(logger)

	rateLimiter := ratelimit.New(
		rate.Limit(cfg.RateLimit.RequestsPerSecond),
		cfg.RateLimit.BurstSize,
		logger,
	)

	// 创建重试器
	retrier := retry.NewDefault(logger).
		WithMaxRetries(cfg.Retry.MaxRetries).
		WithBaseDelay(time.Duration(cfg.Retry.BaseDelay) * time.Second).
		WithMaxDelay(time.Duration(cfg.Retry.MaxDelay) * time.Second)

	// 创建分块下载器
	chunkedDownloader := chunked.New(logger).
		WithChunkSize(cfg.Download.ChunkSize * 1024). // 转换为字节
		WithMaxWorkers(cfg.Download.MaxWorkers)

	c := &Client{
		config:            cfg,
		logger:            logger,
		sessionMgr:        sessionMgr,
		targetChatID:      chatID,
		floodWaiter:       floodWaiter,
		rateLimiter:       rateLimiter,
		retrier:           retrier,
		chunkedDownloader: chunkedDownloader,
	}

	// 创建UpdateDispatcher
	dispatcher := tg.NewUpdateDispatcher()

	// 注册新消息处理器
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
		return c.handleNewMessage(ctx, update, chatID)
	})

	// 注册新频道消息处理器
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateNewChannelMessage) error {
		return c.handleNewChannelMessage(ctx, update, chatID)
	})

	// 使用updates.New创建UpdateHandler
	updateHandler := updates.New(updates.Config{
		Handler: dispatcher,
	})

	// 创建带中间件和Updates处理器的客户端
	tgClient := sessionMgr.CreateClientWithMiddlewareAndUpdates(
		cfg.API.ID,
		cfg.API.Hash,
		cfg.API.Phone,
		updateHandler,
		floodWaiter,
		rateLimiter,
	)

	if tgClient == nil {
		logger.Error("无法创建带Updates处理器的客户端")
		return nil
	}

	c.Client = tgClient
	c.API = tgClient.API()

	// 创建下载器
	c.downloader = downloader.New(tgClient, cfg.Download.Path, cfg.Download.MaxConcurrent, logger)
	c.downloader.SetDownloadFunc(c.DownloadFile)

	logger.Info("已创建带实时监控功能的优化Telegram客户端 (分块下载: %v, 速率限制: %.1f req/s, 重试: %d次)",
		cfg.Download.UseChunked,
		cfg.RateLimit.RequestsPerSecond,
		cfg.Retry.MaxRetries,
	)

	return c
}

// Connect 连接到Telegram
func (c *Client) Connect(ctx context.Context) error {
	// 使用会话管理器重新创建客户端（确保使用最新的会话）
	client := c.sessionMgr.CreateClientWithSession(c.config.API.ID, c.config.API.Hash, c.config.API.Phone)
	c.Client = client
	c.API = client.API()

	// 重新创建下载器
	c.downloader = downloader.New(client, c.config.Download.Path, c.config.Download.MaxConcurrent, c.logger)
	c.downloader.SetDownloadFunc(c.DownloadFile)

	// 连接并认证
	return client.Run(ctx, func(ctx context.Context) error {
		// 检查授权状态
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("检查授权状态失败: %w", err)
		}

		if !status.Authorized {
			// 需要登录
			c.logger.Info("当前未授权，开始登录流程...")
			if err := c.Authenticate(ctx); err != nil {
				return fmt.Errorf("认证失败: %w", err)
			}
			c.logger.Info("登录成功，会话已保存")
		} else {
			c.logger.Info("使用现有会话自动登录成功")
		}

		c.logger.Info("成功连接到Telegram")
		return nil
	})
}

// Authenticate 进行用户认证
func (c *Client) Authenticate(ctx context.Context) error {
	c.logger.Info("开始认证流程...")

	// 发送验证码
	sentCodeClass, err := c.Client.Auth().SendCode(ctx, c.config.API.Phone, auth.SendCodeOptions{})
	if err != nil {
		return fmt.Errorf("发送验证码失败: %w", err)
	}

	sentCode, ok := sentCodeClass.(*tg.AuthSentCode)
	if !ok {
		return errors.New("unexpected sent code type")
	}

	// 提示输入验证码
	fmt.Printf("请输入验证码: ")
	var code string
	if _, scanErr := fmt.Scanln(&code); scanErr != nil {
		return fmt.Errorf("读取验证码失败: %w", scanErr)
	}

	// 进行SignIn
	_, err = c.Client.Auth().SignIn(ctx, c.config.API.Phone, code, sentCode.PhoneCodeHash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		// 提示输入密码
		fmt.Printf("请输入两步验证密码: ")
		var password string
		if _, scanErr := fmt.Scanln(&password); scanErr != nil {
			return fmt.Errorf("读取密码失败: %w", scanErr)
		}

		// 使用密码进行认证
		_, err = c.Client.Auth().Password(ctx, password)
		if err != nil {
			return fmt.Errorf("两步验证失败: %w", err)
		}
		c.logger.Info("两步验证成功")
	} else if err != nil {
		return fmt.Errorf("SignIn失败: %w", err)
	}

	c.logger.Info("认证流程完成")
	return nil
}

// ClearSession 清除保存的会话
func (c *Client) ClearSession() error {
	if err := c.sessionMgr.ClearSession(c.config.API.Phone); err != nil {
		return fmt.Errorf("清除会话失败: %w", err)
	}
	c.logger.Info("会话已清除，下次启动需要重新登录")
	return nil
}

// GetChats 获取聊天列表
func (c *Client) GetChats(ctx context.Context) ([]ChatInfo, error) {
	dialogs, err := c.API.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		Limit: 100,
	})
	if err != nil {
		return nil, fmt.Errorf("获取对话列表失败: %w", err)
	}

	var chats []ChatInfo
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		for _, chat := range d.Chats {
			if info := c.extractChatInfo(chat); info != nil {
				chats = append(chats, *info)
			}
		}
	case *tg.MessagesDialogsSlice:
		for _, chat := range d.Chats {
			if info := c.extractChatInfo(chat); info != nil {
				chats = append(chats, *info)
			}
		}
	}

	return chats, nil
}

// ChatInfo 聊天信息
type ChatInfo struct {
	ID    int64
	Title string
	Type  string
}

// extractChatInfo 提取聊天信息
func (c *Client) extractChatInfo(chat tg.ChatClass) *ChatInfo {
	switch ch := chat.(type) {
	case *tg.Chat:
		return &ChatInfo{
			ID:    ch.ID,
			Title: ch.Title,
			Type:  "群组",
		}
	case *tg.Channel:
		chatType := "频道"
		if ch.Megagroup {
			chatType = "超级群组"
		}
		return &ChatInfo{
			ID:    ch.ID,
			Title: ch.Title,
			Type:  chatType,
		}
	}
	return nil
}

// GetMediaMessages 获取包含媒体的消息
func (c *Client) GetMediaMessages(ctx context.Context, chatID int64, limit, offsetID int) ([]*downloader.MediaInfo, error) {
	// 构建输入对等体
	inputPeer := &tg.InputPeerChat{ChatID: chatID}

	// 获取消息历史
	history, err := c.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:       inputPeer,
		OffsetID:   offsetID,
		OffsetDate: 0,
		AddOffset:  0,
		Limit:      limit,
		MaxID:      0,
		MinID:      0,
		Hash:       0,
	})

	if err != nil {
		return nil, fmt.Errorf("获取消息历史失败: %w", err)
	}

	var mediaList []*downloader.MediaInfo

	switch h := history.(type) {
	case *tg.MessagesMessages:
		for _, msg := range h.Messages {
			if media := c.extractMediaInfo(msg, chatID); media != nil {
				mediaList = append(mediaList, media)
			}
		}
	case *tg.MessagesMessagesSlice:
		for _, msg := range h.Messages {
			if media := c.extractMediaInfo(msg, chatID); media != nil {
				mediaList = append(mediaList, media)
			}
		}
	case *tg.MessagesChannelMessages:
		for _, msg := range h.Messages {
			if media := c.extractMediaInfo(msg, chatID); media != nil {
				mediaList = append(mediaList, media)
			}
		}
	}

	return mediaList, nil
}

// extractMediaInfo 提取媒体信息
func (c *Client) extractMediaInfo(msg tg.MessageClass, chatID int64) *downloader.MediaInfo {
	message, ok := msg.(*tg.Message)
	if !ok {
		return nil
	}

	if message.Media == nil {
		return nil
	}

	switch media := message.Media.(type) {
	case *tg.MessageMediaPhoto:
		return c.extractPhotoInfo(media, message, chatID)
	case *tg.MessageMediaDocument:
		return c.extractDocumentInfo(media, message, chatID)
	default:
		return nil
	}
}

// extractPhotoInfo 提取照片信息
func (c *Client) extractPhotoInfo(media *tg.MessageMediaPhoto, message *tg.Message, chatID int64) *downloader.MediaInfo {
	photo, ok := media.Photo.(*tg.Photo)
	if !ok {
		return nil
	}

	mediaInfo := &downloader.MediaInfo{
		MessageID:     message.ID,
		FileID:        photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		MediaType:     "photo",
		FileName:      fmt.Sprintf("photo_%d.jpg", photo.ID),
		ChatID:        chatID,
		Date:          time.Unix(int64(message.Date), 0),
		MimeType:      "image/jpeg",
	}

	// 获取最大尺寸和ThumbSize
	maxSize, thumbType := c.findLargestPhotoSize(photo.Sizes)
	mediaInfo.FileSize = int64(maxSize)
	mediaInfo.ThumbSize = thumbType

	return mediaInfo
}

// findLargestPhotoSize 查找最大的照片尺寸
func (c *Client) findLargestPhotoSize(sizes []tg.PhotoSizeClass) (maxSize int, thumbType string) {
	for _, size := range sizes {
		currentSize, currentType := c.getPhotoSizeInfo(size)
		if currentSize > maxSize {
			maxSize = currentSize
			thumbType = currentType
		}
	}

	return maxSize, thumbType
}

// getPhotoSizeInfo 获取照片尺寸信息
func (c *Client) getPhotoSizeInfo(size tg.PhotoSizeClass) (width int, url string) {
	switch s := size.(type) {
	case *tg.PhotoSize:
		return s.Size, s.Type
	case *tg.PhotoStrippedSize:
		return len(s.Bytes), s.Type
	case *tg.PhotoSizeProgressive:
		if len(s.Sizes) > 0 {
			return s.Sizes[len(s.Sizes)-1], s.Type
		}
		return 0, s.Type
	default:
		return 0, ""
	}
}

// extractDocumentInfo 提取文档信息
func (c *Client) extractDocumentInfo(media *tg.MessageMediaDocument, message *tg.Message, chatID int64) *downloader.MediaInfo {
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return nil
	}

	fileName := c.getDocumentFileName(doc)

	return &downloader.MediaInfo{
		MessageID:     message.ID,
		FileID:        doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
		MediaType:     "document",
		FileName:      fileName,
		FileSize:      doc.Size,
		ThumbSize:     "",
		ChatID:        chatID,
		Date:          time.Unix(int64(message.Date), 0),
		MimeType:      doc.MimeType,
	}
}

// getDocumentFileName 获取文档文件名
func (c *Client) getDocumentFileName(doc *tg.Document) string {
	// 尝试获取文件名
	for _, attr := range doc.Attributes {
		if filename, ok := attr.(*tg.DocumentAttributeFilename); ok {
			return filename.FileName
		}
	}
	// 如果没有找到文件名，使用默认格式
	return fmt.Sprintf("document_%d", doc.ID)
}

// DownloadFile 实际下载文件
func (c *Client) DownloadFile(ctx context.Context, media *downloader.MediaInfo, filePath string) error {
	// 构建下载位置
	location, err := c.buildFileLocation(media)
	if err != nil {
		return fmt.Errorf("构建下载位置失败: %w", err)
	}

	// 使用重试机制包装下载逻辑
	return c.retrier.Do(ctx, func() error {
		// 检查是否使用分块下载
		if c.config.Download.UseChunked && media.FileSize > 1024*1024 { // 大于1MB使用分块下载
			c.logger.Info("使用分块下载器下载文件: %s (大小: %d bytes)", media.FileName, media.FileSize)
			
			// 创建下载函数
			downloadFunc := func(offset int64, limit int) ([]byte, error) {
				// 调用Telegram API
				fileData, err := c.API.UploadGetFile(ctx, &tg.UploadGetFileRequest{
					Precise:  true,
					Location: location,
					Offset:   offset,
					Limit:    limit,
				})

				if err != nil {
					return nil, err
				}

				if uploadFile, ok := fileData.(*tg.UploadFile); ok {
					return uploadFile.Bytes, nil
				}

				return nil, fmt.Errorf("未知的文件数据类型")
			}

			// 使用分块下载器
			return c.chunkedDownloader.DownloadToFile(ctx, downloadFunc, media.FileSize, filePath)
		} else {
			// 使用传统下载方式
			c.logger.Info("使用传统方式下载文件: %s (大小: %d bytes)", media.FileName, media.FileSize)
			return c.downloadFileTraditional(ctx, location, media.FileSize, filePath)
		}
	})
}

// downloadFileTraditional 传统下载方式
func (c *Client) downloadFileTraditional(ctx context.Context, location tg.InputFileLocationClass, fileSize int64, filePath string) error {
	// 创建临时文件
	tempPath := filePath + ".tmp"

	// 如果临时文件已存在，先删除
	if _, err := os.Stat(tempPath); err == nil {
		c.logger.Warn("发现已存在的临时文件，正在删除: %s", tempPath)
		if removeErr := os.Remove(tempPath); removeErr != nil {
			c.logger.Error("删除已存在临时文件失败: %v", removeErr)
			time.Sleep(100 * time.Millisecond)
			os.Remove(tempPath)
		}
	}

	file, err := c.createTempFile(tempPath)
	if err != nil {
		return err
	}

	// 确保文件被正确关闭
	var downloadErr error

	// 使用匿名函数确保文件被关闭
	func() {
		defer func() {
			if closeErr := file.Close(); closeErr != nil {
				c.logger.Error("关闭文件失败: %v", closeErr)
			}
			time.Sleep(100 * time.Millisecond)
		}()

		// 下载文件
		downloadErr = c.downloadFileChunks(ctx, file, location, fileSize, tempPath)
	}()

	// 检查下载是否成功
	if downloadErr != nil {
		c.logger.Error("下载失败，清理临时文件: %s", tempPath)
		c.cleanupTempFile(tempPath)
		return downloadErr
	}

	// 完成文件处理（重命名）
	c.logger.Info("下载完成，正在重命名文件...")
	return c.finalizeTempFile(tempPath, filePath)
}

// createTempFile 创建临时文件
func (c *Client) createTempFile(tempPath string) (*os.File, error) {
	// 验证路径安全性
	if !c.isSafePath(tempPath) {
		return nil, fmt.Errorf("unsafe temp file path: %s", tempPath)
	}

	// 额外的路径安全检查
	cleanTempPath := filepath.Clean(tempPath)
	if strings.Contains(cleanTempPath, "..") {
		return nil, fmt.Errorf("detected path traversal in temp path: %s", tempPath)
	}

	file, err := os.Create(cleanTempPath)
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	return file, nil
}

// isSafePath 验证文件路径是否安全
func (c *Client) isSafePath(filePath string) bool {
	// 检查路径中是否包含危险的路径遍历字符
	if strings.Contains(filePath, "..") {
		return false
	}

	// 检查是否为绝对路径或包含危险字符
	if strings.HasPrefix(filePath, "/") || strings.Contains(filePath, "\\..\\") || strings.Contains(filePath, "/..") {
		return false
	}

	return true
}

// buildFileLocation 根据媒体类型构建下载位置
func (c *Client) buildFileLocation(media *downloader.MediaInfo) (tg.InputFileLocationClass, error) {
	switch media.MediaType {
	case "photo":
		return &tg.InputPhotoFileLocation{
			ID:            media.FileID,
			AccessHash:    media.AccessHash,
			FileReference: media.FileReference,
			ThumbSize:     media.ThumbSize,
		}, nil
	case "document":
		return &tg.InputDocumentFileLocation{
			ID:            media.FileID,
			AccessHash:    media.AccessHash,
			FileReference: media.FileReference,
			ThumbSize:     media.ThumbSize,
		}, nil
	default:
		return nil, errors.New("unsupported media type")
	}
}

// downloadFileChunksConcurrent 并发分块下载文件
func (c *Client) downloadFileChunksConcurrent(
	ctx context.Context,
	file *os.File,
	location tg.InputFileLocationClass,
	fileSize int64,
	tempPath string,
) error {
	const (
		chunkSize  = 512 * 1024 // 512KB
		maxWorkers = 4          // 并发数
		maxRetries = 3          // 重试次数
	)

	c.logger.Info("开始并发下载，文件大小: %d bytes, 块大小: %d KB, 并发数: %d", fileSize, chunkSize/1024, maxWorkers)

	// 计算总块数
	totalChunks := int((fileSize + int64(chunkSize) - 1) / int64(chunkSize))

	// 创建工作队列和结果通道
	type chunkJob struct {
		offset int64
		size   int
		index  int
	}

	type chunkResult struct {
		index int
		data  []byte
		err   error
	}

	jobs := make(chan chunkJob, totalChunks)
	results := make(chan chunkResult, totalChunks)

	// 启动工作协程
	for i := 0; i < maxWorkers; i++ {
		go func(workerID int) {
			for job := range jobs {
				c.logger.Debug("Worker %d 开始下载块 %d (offset: %d, size: %d)", workerID, job.index, job.offset, job.size)

				var data []byte
				var err error

				// 重试机制
				for retry := 0; retry < maxRetries; retry++ {
					if retry > 0 {
						delay := time.Duration(retry) * time.Second
						c.logger.Debug("Worker %d 重试块 %d (第%d次)", workerID, job.index, retry)
						time.Sleep(delay)
					}

					fileData, downloadErr := c.API.UploadGetFile(ctx, &tg.UploadGetFileRequest{
						Precise:  true,
						Location: location,
						Offset:   job.offset,
						Limit:    job.size,
					})

					if downloadErr == nil {
						if uploadFile, ok := fileData.(*tg.UploadFile); ok {
							data = uploadFile.Bytes
							break
						} else {
							err = fmt.Errorf("未知的文件数据类型")
							break
						}
					}

					// 检查API限制错误
					errStr := downloadErr.Error()
					if strings.Contains(errStr, "LIMIT_INVALID") ||
						strings.Contains(errStr, "FLOOD_WAIT") ||
						strings.Contains(errStr, "420") {
						c.logger.Warn("Worker %d 遇到API限制: %v", workerID, downloadErr)
						if strings.Contains(errStr, "FLOOD_WAIT") {
							time.Sleep(time.Duration(retry+1) * 3 * time.Second)
						}
						err = downloadErr
						continue
					}

					err = downloadErr
				}

				results <- chunkResult{
					index: job.index,
					data:  data,
					err:   err,
				}
			}
		}(i)
	}

	// 发送下载任务
	go func() {
		defer close(jobs)
		const alignment = 4096 // 4KB对齐
		
		for i := 0; i < totalChunks; i++ {
			offset := int64(i) * int64(chunkSize)
			
			// 确保偏移量是1KB对齐的
			if offset%alignment != 0 {
				offset = (offset / alignment) * alignment
			}
			
			size := chunkSize
			if offset+int64(size) > fileSize {
				size = int(fileSize - offset)
			}
			
			// 确保块大小也是4KB对齐的
			if size%alignment != 0 {
				size = (size / alignment) * alignment
				if size == 0 {
					size = alignment // 最小4KB
				}
			}

			jobs <- chunkJob{
				offset: offset,
				size:   size,
				index:  i,
			}
		}
	}()

	// 收集结果并写入文件
	chunks := make([][]byte, totalChunks)
	var completedChunks int
	var totalBytes int64

	for i := 0; i < totalChunks; i++ {
		result := <-results
		if result.err != nil {
			return fmt.Errorf("下载块 %d 失败: %v", result.index, result.err)
		}

		chunks[result.index] = result.data
		completedChunks++
		totalBytes += int64(len(result.data))

		// 显示进度
		progress := float64(completedChunks) / float64(totalChunks) * 100
		if completedChunks%10 == 0 || completedChunks == totalChunks {
			c.logger.Info("下载进度: %.1f%% (%d/%d 块)", progress, completedChunks, totalChunks)
		}
	}

	// 按顺序写入文件
	c.logger.Info("开始写入文件...")
	for i, chunk := range chunks {
		if chunk == nil {
			return fmt.Errorf("块 %d 数据为空", i)
		}

		_, err := file.Write(chunk)
		if err != nil {
			return fmt.Errorf("写入块 %d 失败: %v", i, err)
		}
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步文件失败: %w", err)
	}
	c.logger.Info("并发下载完成，总大小: %d bytes", totalBytes)
	return nil
}
func (c *Client) downloadFileChunks(
	ctx context.Context,
	file *os.File,
	location tg.InputFileLocationClass,
	fileSize int64,
	tempPath string,
) error {
	const (
		chunkSize  = 512 * 1024      // 512KB
		alignment  = 1024            // 1KB - Telegram API要求
		maxRetries = 3               // 减少重试次数，依赖重试器
		baseDelay  = 1 * time.Second // 减少基础延迟
	)
	var offset int64

	c.logger.Info("开始分块下载，文件大小: %d bytes, 块大小: %d KB", fileSize, chunkSize/1024)

	for offset < fileSize {
		// 确保偏移量是4KB对齐的
		if offset%alignment != 0 {
			offset = (offset / alignment) * alignment
		}
		
		limit := c.calculateChunkLimit(chunkSize, fileSize-offset)

		// 调用Telegram API获取文件块
		fileData, err := c.API.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Precise:  true,
			Location: location,
			Offset:   offset,
			Limit:    limit,
		})

		if err != nil {
			return fmt.Errorf("下载块失败 (偏移: %d, 大小: %d): %w", offset, limit, err)
		}

		// 写入文件块
		bytesWritten, err := c.writeFileChunk(file, fileData)
		if err != nil {
			return err
		}

		// 确保下一个偏移量也是1KB对齐的
		nextOffset := offset + int64(bytesWritten)
		if nextOffset%alignment != 0 {
			nextOffset = ((nextOffset / alignment) + 1) * alignment
		}
		offset = nextOffset

		// 减少进度日志频率，避免影响性能
		if offset%(1024*1024) == 0 || offset >= fileSize { // 每1MB或完成时显示
			progress := float64(offset) / float64(fileSize) * 100
			c.logger.Info("下载进度: %.1f%% (%d/%d bytes)", progress, offset, fileSize)
		}
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步文件失败: %w", err)
	}
	c.logger.Info("文件下载完成，总大小: %d bytes", offset)
	return nil
}

// calculateChunkLimit 计算块大小限制
func (c *Client) calculateChunkLimit(chunkSize int, remaining int64) int {
	const alignment = 1024 // 1KB - Telegram API要求offset和limit都必须是1KB的倍数
	
	limit := chunkSize
	if remaining < int64(chunkSize) {
		limit = int(remaining)
	}

	// Telegram API限制：最大512KB，符合upload.getFile的limit参数限制
	maxLimit := 512 * 1024 // 512KB - Telegram API最大限制
	if limit > maxLimit {
		limit = maxLimit
	}

	// 确保是1KB的倍数，符合Telegram API要求
	if limit%alignment != 0 {
		limit = (limit / alignment) * alignment
		if limit == 0 {
			limit = alignment // 最小1KB
		}
	}

	return limit
}

// writeFileChunk 写入文件块
func (c *Client) writeFileChunk(file *os.File, fileData tg.UploadFileClass) (int, error) {
	switch fd := fileData.(type) {
	case *tg.UploadFile:
		_, err := file.Write(fd.Bytes)
		if err != nil {
			return 0, fmt.Errorf("写入文件失败: %w", err)
		}
		return len(fd.Bytes), nil
	default:
		return 0, fmt.Errorf("未知的文件数据类型")
	}
}

// cleanupTempFile 清理临时文件
func (c *Client) cleanupTempFile(tempPath string) {
	if removeErr := os.Remove(tempPath); removeErr != nil {
		c.logger.Error("清理临时文件失败: %v", removeErr)
	}
}

// finalizeTempFile 完成临时文件处理
func (c *Client) finalizeTempFile(tempPath, filePath string) error {
	if _, err := os.Stat(tempPath); os.IsNotExist(err) {
		return fmt.Errorf("临时文件不存在: %s", tempPath)
	}
	var renameErr error
	for retry := 0; retry < 5; retry++ {
		renameErr = os.Rename(tempPath, filePath)
		if renameErr == nil {
			return nil
		}
		c.logger.Warn("重命名失败 (尝试 %d): %v", retry+1, renameErr)
		time.Sleep(500 * time.Millisecond)
	}
	c.cleanupTempFile(tempPath)
	return fmt.Errorf("重命名文件失败 after retries: %w", renameErr)
}

// ManualCheckNewMessages 手动检查新消息
func (c *Client) ManualCheckNewMessages(ctx context.Context, chatID int64) error {
	c.logger.Info("开始手动检查聊天 %d 的新消息", chatID)

	// 获取当前最新消息ID
	latestID := c.getLastMessageID(ctx, chatID)
	if latestID == 0 {
		return fmt.Errorf("无法获取最新消息ID")
	}

	c.logger.Info("当前最新消息ID: %d", latestID)

	// 如果没有保存的lastMessageID，使用当前最新的
	if c.lastMessageID == 0 {
		c.lastMessageID = latestID
		c.logger.Info("初始化lastMessageID为: %d", c.lastMessageID)
		return nil
	}

	// 检查是否有新消息
	if latestID > c.lastMessageID {
		c.logger.Info("发现新消息！从 %d 到 %d", c.lastMessageID, latestID)

		// 检查新消息
		err := c.checkForNewMessages(ctx, c.lastMessageID)
		if err != nil {
			return fmt.Errorf("检查新消息失败: %w", err)
		}

		// 更新lastMessageID
		c.lastMessageID = latestID
	} else {
		c.logger.Info("没有新消息")
	}

	return nil
}

// startRealTimeMonitor 启动实时Updates监控（已废弃，保留兼容性）
func (c *Client) startRealTimeMonitor() {
	c.logger.Info("实时监控已通过Updates处理器启动")
}

// handleUpdates 处理Telegram Updates
func (c *Client) handleUpdates(ctx context.Context, updates tg.UpdatesClass) error {
	switch u := updates.(type) {
	case *tg.Updates:
		for _, update := range u.Updates {
			if err := c.processUpdate(ctx, update); err != nil {
				c.logger.Error("处理更新失败: %v", err)
			}
		}
	case *tg.UpdateShort:
		if err := c.processUpdate(ctx, u.Update); err != nil {
			c.logger.Error("处理短更新失败: %v", err)
		}
	case *tg.UpdateShortMessage:
		// 处理短消息更新
		c.logger.Debug("收到短消息更新，消息ID: %d", u.ID)
	case *tg.UpdateShortChatMessage:
		// 处理短聊天消息更新
		c.logger.Debug("收到短聊天消息更新，消息ID: %d", u.ID)
	}

	return nil
}

// processUpdate 处理单个Update
func (c *Client) processUpdate(ctx context.Context, update tg.UpdateClass) error {
	switch u := update.(type) {
	case *tg.UpdateNewMessage:
		c.logger.Debug("收到新消息更新")
		return c.handleNewMessage(ctx, u, c.targetChatID)
	case *tg.UpdateNewChannelMessage:
		c.logger.Debug("收到新频道消息更新")
		return c.handleNewChannelMessage(ctx, u, c.targetChatID)
	default:
		// 忽略其他类型的更新
		return nil
	}
}

// getLastMessageID 获取聊天中最新消息的ID
func (c *Client) getLastMessageID(ctx context.Context, chatID int64) int {
	inputPeer := &tg.InputPeerChat{ChatID: chatID}

	history, err := c.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     inputPeer,
		OffsetID: 0,
		Limit:    1,
	})

	if err != nil {
		c.logger.Error("获取最新消息ID失败: %v", err)
		return 0
	}

	switch h := history.(type) {
	case *tg.MessagesMessages:
		if len(h.Messages) > 0 {
			if msg, ok := h.Messages[0].(*tg.Message); ok {
				return msg.ID
			}
		}
	case *tg.MessagesMessagesSlice:
		if len(h.Messages) > 0 {
			if msg, ok := h.Messages[0].(*tg.Message); ok {
				return msg.ID
			}
		}
	}

	return 0
}

// checkForNewMessages 检查新消息并下载媒体
func (c *Client) checkForNewMessages(ctx context.Context, lastMessageID int) error {
	inputPeer := &tg.InputPeerChat{ChatID: c.targetChatID}

	c.logger.Debug("检查聊天 %d 中比消息ID %d 更新的消息", c.targetChatID, lastMessageID)

	// 获取比lastMessageID更新的消息
	// OffsetID为0表示从最新消息开始获取
	history, err := c.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     inputPeer,
		OffsetID: 0,  // 从最新消息开始
		Limit:    20, // 增加检查数量
	})

	if err != nil {
		return fmt.Errorf("获取消息历史失败: %w", err)
	}

	var newMessages []tg.MessageClass
	var totalCount int

	switch h := history.(type) {
	case *tg.MessagesMessages:
		newMessages = h.Messages
		totalCount = len(h.Messages)
	case *tg.MessagesMessagesSlice:
		newMessages = h.Messages
		totalCount = h.Count
	}

	c.logger.Debug("获取到 %d 条消息，总计 %d 条", len(newMessages), totalCount)

	newMessageCount := 0
	mediaMessageCount := 0

	// 处理新消息
	for i, msgClass := range newMessages {
		c.logger.Debug("处理消息 %d/%d", i+1, len(newMessages))

		if msg, ok := msgClass.(*tg.Message); ok {
			c.logger.Debug("消息ID: %d, lastMessageID: %d", msg.ID, lastMessageID)

			// 只处理比lastMessageID更新的消息
			if msg.ID > lastMessageID {
				newMessageCount++
				c.logger.Info("发现新消息 ID: %d, 内容: %s", msg.ID, c.getMessagePreview(msg))

				// 检查消息是否包含媒体
				c.logger.Debug("检查消息 %d 是否包含媒体", msg.ID)
				if c.hasMedia(msg) {
					mediaMessageCount++
					c.logger.Info("新消息包含媒体，开始下载...")

					// 创建媒体信息并下载
					c.logger.Debug("创建媒体信息...")
					mediaInfo := c.createMediaInfo(msg)
					if mediaInfo != nil {
						c.logger.Info("媒体信息创建成功: %+v", mediaInfo)
						c.logger.Info("调用下载器下载媒体...")
						c.downloader.DownloadSingle(ctx, mediaInfo)
						c.logger.Info("媒体下载任务已提交: %s", mediaInfo.FileName)
					} else {
						c.logger.Error("无法创建媒体信息")
					}
				} else {
					c.logger.Debug("新消息不包含媒体，媒体类型: %T", msg.Media)
				}
			} else {
				c.logger.Debug("跳过旧消息 ID: %d (不大于 %d)", msg.ID, lastMessageID)
			}
		} else {
			c.logger.Debug("消息类型不是 *tg.Message: %T", msgClass)
		}
	}

	if newMessageCount > 0 {
		c.logger.Info("本次检查发现 %d 条新消息，其中 %d 条包含媒体", newMessageCount, mediaMessageCount)
	} else {
		c.logger.Debug("本次检查未发现新消息")
	}

	return nil
}

// getMessagePreview 获取消息预览文本
func (c *Client) getMessagePreview(msg *tg.Message) string {
	if msg.Message != "" {
		if len(msg.Message) > 50 {
			return msg.Message[:50] + "..."
		}
		return msg.Message
	}

	if msg.Media != nil {
		switch msg.Media.(type) {
		case *tg.MessageMediaPhoto:
			return "[图片]"
		case *tg.MessageMediaDocument:
			return "[文档]"
		default:
			return "[媒体]"
		}
	}

	return "[空消息]"
}

// hasMedia 检查消息是否包含媒体
func (c *Client) hasMedia(msg *tg.Message) bool {
	return msg.Media != nil
}

// createMediaInfo 从消息创建媒体信息
func (c *Client) createMediaInfo(msg *tg.Message) *downloader.MediaInfo {
	if msg.Media == nil {
		return nil
	}

	switch media := msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		if photo, ok := media.Photo.(*tg.Photo); ok {
			// 获取最大尺寸的照片
			var maxSize *tg.PhotoSize
			var fileSize int64
			for _, size := range photo.Sizes {
				if photoSize, ok := size.(*tg.PhotoSize); ok {
					if maxSize == nil || photoSize.Size > maxSize.Size {
						maxSize = photoSize
						fileSize = int64(photoSize.Size)
					}
				}
			}

			return &downloader.MediaInfo{
				MessageID:     msg.ID,
				FileID:        photo.ID,
				AccessHash:    photo.AccessHash,
				FileReference: photo.FileReference,
				FileName:      fmt.Sprintf("photo_%d_%d.jpg", c.targetChatID, msg.ID),
				FileSize:      fileSize,
				MediaType:     "photo",
				ChatID:        c.targetChatID,
				Date:          time.Unix(int64(msg.Date), 0),
			}
		}
	case *tg.MessageMediaDocument:
		if doc, ok := media.Document.(*tg.Document); ok {
			fileName := fmt.Sprintf("document_%d_%d", c.targetChatID, msg.ID)

			// 尝试从属性中获取文件名
			for _, attr := range doc.Attributes {
				if docAttr, ok := attr.(*tg.DocumentAttributeFilename); ok {
					fileName = docAttr.FileName
					break
				}
			}

			return &downloader.MediaInfo{
				MessageID:     msg.ID,
				FileID:        doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
				FileName:      fileName,
				FileSize:      doc.Size,
				MediaType:     "document",
				MimeType:      doc.MimeType,
				ChatID:        c.targetChatID,
				Date:          time.Unix(int64(msg.Date), 0),
			}
		}
	}

	return nil
}

// handleNewMessage 处理新消息
func (c *Client) handleNewMessage(ctx context.Context, update *tg.UpdateNewMessage, targetChatID int64) error {
	message, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}

	// 先检查是否来自目标聊天，避免刷屏
	if !c.isFromTargetChat(message, targetChatID) {
		return nil
	}

	// 只有来自目标聊天的消息才显示日志
	c.logger.Info("🔔 收到目标聊天的新消息！")
	c.logger.Info("📨 处理消息 ID: %d", message.ID)

	// 检查消息是否包含媒体
	if !c.hasMedia(message) {
		c.logger.Info("📝 消息不包含媒体，内容: %s", c.getMessagePreview(message))
		return nil
	}

	c.logger.Info("🎬 检测到新媒体消息，消息ID: %d, 内容: %s", message.ID, c.getMessagePreview(message))

	// 创建媒体信息
	mediaInfo := c.createMediaInfo(message)
	if mediaInfo == nil {
		c.logger.Error("无法创建媒体信息")
		return nil
	}

	c.logger.Info("媒体信息创建成功: %+v", mediaInfo)

	// 下载媒体文件
	go func() {
		downloadCtx := context.Background()
		c.logger.Info("开始下载媒体文件: %s", mediaInfo.FileName)
		c.downloader.DownloadSingle(downloadCtx, mediaInfo)
		c.logger.Info("媒体文件下载任务已提交: %s", mediaInfo.FileName)
	}()

	return nil
}

// handleNewChannelMessage 处理频道新消息
func (c *Client) handleNewChannelMessage(ctx context.Context, update *tg.UpdateNewChannelMessage, targetChatID int64) error {
	message, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}

	// 先检查是否来自目标频道，避免刷屏
	if !c.isFromTargetChat(message, targetChatID) {
		return nil
	}

	// 只有来自目标频道的消息才显示日志
	c.logger.Info("🔔 收到目标频道的新消息！")
	c.logger.Info("📨 处理频道消息 ID: %d", message.ID)

	// 检查消息是否包含媒体
	if !c.hasMedia(message) {
		c.logger.Info("📝 频道消息不包含媒体，内容: %s", c.getMessagePreview(message))
		return nil
	}

	c.logger.Info("🎬 检测到频道新媒体消息，消息ID: %d, 内容: %s", message.ID, c.getMessagePreview(message))

	// 创建媒体信息
	mediaInfo := c.createMediaInfo(message)
	if mediaInfo == nil {
		c.logger.Error("无法创建媒体信息")
		return nil
	}

	c.logger.Info("媒体信息创建成功: %+v", mediaInfo)

	// 下载媒体文件
	go func() {
		downloadCtx := context.Background()
		c.logger.Info("开始下载媒体文件: %s", mediaInfo.FileName)
		c.downloader.DownloadSingle(downloadCtx, mediaInfo)
		c.logger.Info("媒体文件下载任务已提交: %s", mediaInfo.FileName)
	}()

	return nil
}

// isFromTargetChat 检查消息是否来自目标聊天
func (c *Client) isFromTargetChat(message *tg.Message, targetChatID int64) bool {
	if message.PeerID == nil {
		return false
	}

	switch peer := message.PeerID.(type) {
	case *tg.PeerChannel:
		return peer.ChannelID == targetChatID
	case *tg.PeerChat:
		return peer.ChatID == targetChatID
	case *tg.PeerUser:
		return peer.UserID == targetChatID
	default:
		return false
	}
}

// DownloadHistoryMedia 下载历史媒体文件
func (c *Client) DownloadHistoryMedia(ctx context.Context, chatID int64) error {
	c.logger.Info("开始下载聊天 %d 的历史媒体文件", chatID)

	batchSize := c.config.Download.BatchSize
	offsetID := 0
	totalDownloaded := 0

	for {
		mediaList, err := c.GetMediaMessages(ctx, chatID, batchSize, offsetID)
		if err != nil {
			return fmt.Errorf("获取媒体消息失败: %w", err)
		}

		if len(mediaList) == 0 {
			break
		}

		c.logger.Info("获取到 %d 个媒体文件，开始下载...", len(mediaList))
		c.downloader.DownloadBatch(ctx, mediaList)
		c.downloader.Wait() // 等待当前批次完成

		totalDownloaded += len(mediaList)
		c.logger.Info("已处理 %d 个媒体文件", totalDownloaded)

		// 更新偏移量
		if len(mediaList) > 0 {
			offsetID = mediaList[len(mediaList)-1].MessageID
		}

		// 如果返回的数量少于批次大小，说明已经到达末尾
		if len(mediaList) < batchSize {
			break
		}
	}

	c.logger.Info("历史媒体文件下载完成，总计处理 %d 个文件", totalDownloaded)
	c.downloader.PrintStats()
	return nil
}

// GetDownloader 获取下载器实例
func (c *Client) GetDownloader() *downloader.Downloader {
	return c.downloader
}
