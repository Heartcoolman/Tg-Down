package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
)

// Client Telegram客户端包装器
type Client struct {
	Client     *telegram.Client
	API        *tg.Client
	config     *config.Config
	logger     *logger.Logger
	downloader *downloader.Downloader
}

// New 创建新的Telegram客户端
func New(cfg *config.Config, logger *logger.Logger) *Client {
	tgClient := telegram.NewClient(cfg.API.ID, cfg.API.Hash, telegram.Options{})
	c := &Client{
		Client: tgClient,
		API: tgClient.API(),
		config: cfg,
		logger: logger,
	}
	c.downloader = downloader.New(tgClient, cfg.Download.Path, cfg.Download.MaxConcurrent, logger)
	c.downloader.SetDownloadFunc(c.DownloadFile)
	return c
}

// Connect 连接到Telegram
func (c *Client) Connect(ctx context.Context) error {
	// 创建客户端
	client := telegram.NewClient(c.config.API.ID, c.config.API.Hash, telegram.Options{})
	c.Client = client
	c.API = client.API()

	// 创建下载器
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
			if err := c.Authenticate(ctx); err != nil {
				return fmt.Errorf("认证失败: %w", err)
			}
		}

		c.logger.Info("成功连接到Telegram")
		return nil
	})
}

// authenticate 进行用户认证
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
	if _, err := fmt.Scanln(&code); err != nil {
		return fmt.Errorf("读取验证码失败: %w", err)
	}

	// 进行SignIn
	_, err = c.Client.Auth().SignIn(ctx, c.config.API.Phone, code, sentCode.PhoneCodeHash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		// 提示输入密码
		fmt.Printf("请输入两步验证密码: ")
		var password string
		if _, err := fmt.Scanln(&password); err != nil {
			return fmt.Errorf("读取密码失败: %w", err)
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
		Peer:      inputPeer,
		OffsetID:  offsetID,
		OffsetDate: 0,
		AddOffset: 0,
		Limit:     limit,
		MaxID:     0,
		MinID:     0,
		Hash:      0,
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

	var mediaInfo *downloader.MediaInfo

	switch media := message.Media.(type) {
	case *tg.MessageMediaPhoto:
		if photo, ok := media.Photo.(*tg.Photo); ok {
			mediaInfo = &downloader.MediaInfo{
				MessageID: message.ID,
				FileID:    photo.ID,
				AccessHash: photo.AccessHash,
				FileReference: photo.FileReference,
				MediaType: "photo",
				FileName:  fmt.Sprintf("photo_%d.jpg", photo.ID),
				ChatID:    chatID,
				Date:      time.Unix(int64(message.Date), 0),
				MimeType:  "image/jpeg",
			}
			
			// 获取最大尺寸和ThumbSize
			var maxSize int
			var thumbType string
			for _, size := range photo.Sizes {
				switch s := size.(type) {
				case *tg.PhotoSize:
					if s.Size > maxSize {
						maxSize = s.Size
						thumbType = s.Type
					}
				case *tg.PhotoStrippedSize:
					if len(s.Bytes) > maxSize {
						maxSize = len(s.Bytes)
						thumbType = s.Type
					}
				case *tg.PhotoSizeProgressive:
					if len(s.Sizes) > 0 && s.Sizes[len(s.Sizes)-1] > maxSize {
						maxSize = s.Sizes[len(s.Sizes)-1]
						thumbType = s.Type
					}
				}
			}
			mediaInfo.FileSize = int64(maxSize)
			mediaInfo.ThumbSize = thumbType
		}

	case *tg.MessageMediaDocument:
		if doc, ok := media.Document.(*tg.Document); ok {
			fileName := fmt.Sprintf("document_%d", doc.ID)
			
			// 尝试获取文件名
			for _, attr := range doc.Attributes {
				if filename, ok := attr.(*tg.DocumentAttributeFilename); ok {
					fileName = filename.FileName
					break
				}
			}

			mediaInfo = &downloader.MediaInfo{
				MessageID: message.ID,
				FileID:    doc.ID,
				AccessHash: doc.AccessHash,
				FileReference: doc.FileReference,
				MediaType: "document",
				FileName:  fileName,
				FileSize:  doc.Size,
				ThumbSize: "",
				ChatID:    chatID,
				Date:      time.Unix(int64(message.Date), 0),
				MimeType:  doc.MimeType,
			}
		}
	}

	return mediaInfo
}

// DownloadFile 实际下载文件
func (c *Client) DownloadFile(ctx context.Context, media *downloader.MediaInfo, filePath string) error {
	// 创建临时文件
	tempPath := filePath + ".tmp"
	file, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer file.Close()

	// 根据媒体类型构建下载位置
var location tg.InputFileLocationClass
if media.MediaType == "photo" {
	location = &tg.InputPhotoFileLocation{
		ID:            media.FileID,
		AccessHash:    media.AccessHash,
		FileReference: media.FileReference,
		ThumbSize:     media.ThumbSize,
	}
} else if media.MediaType == "document" {
	location = &tg.InputDocumentFileLocation{
		ID:            media.FileID,
		AccessHash:    media.AccessHash,
		FileReference: media.FileReference,
		ThumbSize:     media.ThumbSize,
	}
} else {
	return errors.New("unsupported media type")
}
	
	// 分块下载文件
	const chunkSize = 512 * 1024 // 512KB
	var offset int64 = 0
	
	for offset < media.FileSize {
		limit := chunkSize
		if remaining := media.FileSize - offset; remaining < chunkSize {
			limit = int(remaining)
		}

		// 下载文件块
		fileData, err := c.API.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Precise: true,
			Location: location,
			Offset:   offset,
			Limit:    limit,
		})

		if err != nil {
			os.Remove(tempPath) // 清理临时文件
			return fmt.Errorf("下载文件块失败: %w", err)
		}

		// 写入文件
		switch fd := fileData.(type) {
		case *tg.UploadFile:
			if _, err := file.Write(fd.Bytes); err != nil {
				os.Remove(tempPath)
				return fmt.Errorf("写入文件失败: %w", err)
			}
			offset += int64(len(fd.Bytes))
		default:
			os.Remove(tempPath)
			return fmt.Errorf("未知的文件数据类型")
		}
	}

	// 关闭文件
	file.Close()

	// 重命名临时文件
	if err := os.Rename(tempPath, filePath); err != nil {
		os.Remove(tempPath)
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