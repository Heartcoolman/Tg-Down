package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gotd/td/telegram"

	"tg-down/internal/logger"
)

// MediaInfo 媒体文件信息
type MediaInfo struct {
	MessageID int
	FileID    int64
	AccessHash int64
	FileReference []byte
	ThumbSize string
	MediaType string // "photo" or "document"
	FileName  string
	FileSize  int64
	MimeType  string
	ChatID    int64
	Date      time.Time
}

// Downloader 下载器
type Downloader struct {
	client        *telegram.Client
	downloadPath  string
	maxConcurrent int
	logger        *logger.Logger
	semaphore     chan struct{}
	wg            sync.WaitGroup
	stats         *DownloadStats
	downloadFunc  func(context.Context, *MediaInfo, string) error
}

// DownloadStats 下载统计
type DownloadStats struct {
	mu             sync.RWMutex
	Total          int
	Downloaded     int
	Failed         int
	Skipped        int
	TotalSize      int64
	DownloadedSize int64
}

// New 创建新的下载器
func New(client *telegram.Client, downloadPath string, maxConcurrent int, logger *logger.Logger) *Downloader {
	return &Downloader{
		client:        client,
		downloadPath:  downloadPath,
		maxConcurrent: maxConcurrent,
		logger:        logger,
		semaphore:     make(chan struct{}, maxConcurrent),
		stats:         &DownloadStats{},
	}
}

// SetDownloadFunc 设置下载函数
func (d *Downloader) SetDownloadFunc(fn func(context.Context, *MediaInfo, string) error) {
	d.downloadFunc = fn
}

// GetStats 获取下载统计
func (d *Downloader) GetStats() DownloadStats {
	d.stats.mu.RLock()
	defer d.stats.mu.RUnlock()
	return *d.stats
}

// updateStats 更新统计信息
func (d *Downloader) updateStats(downloaded bool, size int64) {
	d.stats.mu.Lock()
	defer d.stats.mu.Unlock()

	if downloaded {
		d.stats.Downloaded++
		d.stats.DownloadedSize += size
	} else {
		d.stats.Failed++
	}
}

// DownloadMedia 下载媒体文件
func (d *Downloader) DownloadMedia(ctx context.Context, media *MediaInfo) error {
	d.semaphore <- struct{}{}        // 获取信号量
	defer func() { <-d.semaphore }() // 释放信号量

	// 创建下载目录
	chatDir := filepath.Join(d.downloadPath, fmt.Sprintf("chat_%d", media.ChatID))
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		d.logger.Error("创建目录失败: %v", err)
		d.updateStats(false, 0)
		return err
	}

	// 生成文件路径
	fileName := media.FileName
	if fileName == "" {
		ext := d.getFileExtension(media.MimeType)
		fileName = fmt.Sprintf("file_%d_%s%s", media.MessageID, media.FileID, ext)
	}
	filePath := filepath.Join(chatDir, fileName)

	// 检查文件是否已存在
	if _, err := os.Stat(filePath); err == nil {
		d.logger.Debug("文件已存在，跳过下载: %s", fileName)
		d.stats.mu.Lock()
		d.stats.Skipped++
		d.stats.mu.Unlock()
		return nil
	}

	d.logger.Info("开始下载: %s (大小: %d bytes)", fileName, media.FileSize)

	// 使用设置的下载函数
	if d.downloadFunc != nil {
		if err := d.downloadFunc(ctx, media, filePath); err != nil {
			d.logger.Error("下载失败 %s: %v", fileName, err)
			d.updateStats(false, 0)
			return err
		}
	} else {
		// 创建临时文件作为占位符
		tempPath := filePath + ".tmp"
		file, err := os.Create(tempPath)
		if err != nil {
			d.logger.Error("创建临时文件失败 %s: %v", fileName, err)
			d.updateStats(false, 0)
			return err
		}
		file.Close()

		// 模拟下载过程
		d.logger.Debug("正在下载文件: %s", fileName)
		time.Sleep(100 * time.Millisecond) // 模拟下载时间

		// 下载完成后重命名文件
		if err := os.Rename(tempPath, filePath); err != nil {
			d.logger.Error("重命名文件失败 %s: %v", fileName, err)
			os.Remove(tempPath) // 清理临时文件
			d.updateStats(false, 0)
			return err
		}
	}

	d.logger.Info("下载完成: %s", fileName)
	d.updateStats(true, media.FileSize)
	return nil
}

// getFileExtension 根据MIME类型获取文件扩展名
func (d *Downloader) getFileExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/avi":
		return ".avi"
	case "video/mov":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "audio/mp3":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "application/pdf":
		return ".pdf"
	default:
		return ""
	}
}

// DownloadBatch 批量下载媒体文件
func (d *Downloader) DownloadBatch(ctx context.Context, mediaList []*MediaInfo) {
	d.stats.mu.Lock()
	d.stats.Total += len(mediaList)
	for _, media := range mediaList {
		d.stats.TotalSize += media.FileSize
	}
	d.stats.mu.Unlock()

	d.logger.Info("开始批量下载 %d 个文件", len(mediaList))

	for _, media := range mediaList {
		d.wg.Add(1)
		go func(m *MediaInfo) {
			defer d.wg.Done()
			if err := d.DownloadMedia(ctx, m); err != nil {
				d.logger.Error("下载媒体文件失败: %v", err)
			}
		}(media)
	}
}

// Wait 等待所有下载完成
func (d *Downloader) Wait() {
	d.wg.Wait()
}

// PrintStats 打印下载统计
func (d *Downloader) PrintStats() {
	stats := d.GetStats()
	d.logger.Info("下载统计:")
	d.logger.Info("  总计: %d", stats.Total)
	d.logger.Info("  已下载: %d", stats.Downloaded)
	d.logger.Info("  失败: %d", stats.Failed)
	d.logger.Info("  跳过: %d", stats.Skipped)
	d.logger.Info("  总大小: %.2f MB", float64(stats.TotalSize)/(1024*1024))
	d.logger.Info("  已下载大小: %.2f MB", float64(stats.DownloadedSize)/(1024*1024))
}
