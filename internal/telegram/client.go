// Package telegram provides Telegram client functionality for Tg-Down application.
// It handles authentication, chat management, and media downloading operations.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/session"
)

// Client Telegram客户端包装器
type Client struct {
	Client     *telegram.Client
	API        *tg.Client
	config     *config.Config
	logger     *logger.Logger
	downloader *downloader.Downloader
	sessionMgr *session.Manager
}

// New 创建新的Telegram客户端
func New(cfg *config.Config, logger *logger.Logger) *Client {
	// 创建会话管理器
	sessionMgr := session.New(cfg.Session.Dir, logger)

	// 使用会话管理器创建客户端
	tgClient := sessionMgr.CreateClientWithSession(cfg.API.ID, cfg.API.Hash, cfg.API.Phone)

	c := &Client{
		Client:     tgClient,
		API:        tgClient.API(),
		config:     cfg,
		logger:     logger,
		sessionMgr: sessionMgr,
	}
	c.downloader = downloader.New(tgClient, cfg.Download.Path, cfg.Download.MaxConcurrent, logger)
	c.downloader.SetDownloadFunc(c.DownloadFile)

	// 检查是否有现有会话
	if sessionMgr.HasValidSession(cfg.API.Phone) {
		logger.Info("发现现有会话文件，将尝试自动登录")
	} else {
		logger.Info("未发现会话文件，需要进行首次登录")
	}

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
func (c *Client) GetMediaMessages(ctx context.Context, chatID int64, limit int, offsetID int) ([]*downloader.MediaInfo, error) {
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
func (c *Client) findLargestPhotoSize(sizes []tg.PhotoSizeClass) (int, string) {
	var maxSize int
	var thumbType string

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
func (c *Client) getPhotoSizeInfo(size tg.PhotoSizeClass) (int, string) {
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
	// 创建临时文件
	tempPath := filePath + ".tmp"
	file, err := c.createTempFile(tempPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			c.logger.Error("关闭文件失败: %v", err)
		}
	}()

	// 构建下载位置
	location, err := c.buildFileLocation(media)
	if err != nil {
		return err
	}

	// 下载文件内容
	if err := c.downloadFileChunks(ctx, file, location, media.FileSize, tempPath); err != nil {
		return err
	}

	// 关闭文件
	if err := file.Close(); err != nil {
		c.logger.Error("关闭文件失败: %v", err)
	}

	// 重命名临时文件
	return c.finalizeTempFile(tempPath, filePath)
}

// createTempFile 创建临时文件
func (c *Client) createTempFile(tempPath string) (*os.File, error) {
	// 验证路径安全性
	if !c.isSafePath(tempPath) {
		return nil, fmt.Errorf("unsafe temp file path: %s", tempPath)
	}

	file, err := os.Create(tempPath)
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

// downloadFileChunks 分块下载文件
func (c *Client) downloadFileChunks(ctx context.Context, file *os.File, location tg.InputFileLocationClass, fileSize int64, tempPath string) error {
	const chunkSize = 256 * 1024 // 256KB，更安全的块大小
	var offset int64 = 0

	for offset < fileSize {
		limit := c.calculateChunkLimit(chunkSize, fileSize-offset)

		// 下载文件块
		fileData, err := c.API.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Precise:  false, // 设置为false可能更稳定
			Location: location,
			Offset:   offset,
			Limit:    limit,
		})

		if err != nil {
			c.cleanupTempFile(tempPath)
			return fmt.Errorf("下载文件块失败: %w", err)
		}

		// 写入文件块
		bytesWritten, err := c.writeFileChunk(file, fileData)
		if err != nil {
			c.cleanupTempFile(tempPath)
			return err
		}

		offset += int64(bytesWritten)
	}

	return nil
}

// calculateChunkLimit 计算块大小限制
func (c *Client) calculateChunkLimit(chunkSize int, remaining int64) int {
	limit := chunkSize
	if remaining < int64(chunkSize) {
		limit = int(remaining)
	}

	// 确保limit是1024的倍数，且不超过1MB
	if limit > 1024*1024 {
		limit = 1024 * 1024
	}
	if limit%1024 != 0 {
		limit = (limit/1024 + 1) * 1024
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
	if err := os.Rename(tempPath, filePath); err != nil {
		c.cleanupTempFile(tempPath)
		return fmt.Errorf("重命名文件失败: %w", err)
	}
	return nil
}

// SetupRealTimeMonitoring 设置实时监控新消息的更新处理程序
func (c *Client) SetupRealTimeMonitoring(chatID int64) {
	// 注意：实时监控功能需要在主运行循环中实现
	// 这里只是一个占位符，实际的更新处理需要在主程序中设置
	c.logger.Info("实时监控已设置，监听聊天 %d 的新消息", chatID)
	c.logger.Warn("实时监控功能需要在主程序运行循环中实现")
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
