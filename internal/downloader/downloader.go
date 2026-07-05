// Package downloader provides media file downloading functionality for Tg-Down application.
// It supports concurrent downloads with progress tracking and statistics.
package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tg-down/internal/logger"
)

const (
	// DirectoryPermission is the permission mode for creating directories
	DirectoryPermission = 0750
	// MegabyteDivisor is the divisor for converting bytes to megabytes
	MegabyteDivisor = 1024 * 1024
)

// 媒体类型分类目录名
const (
	mediaTypePhoto     = "photo"
	mediaTypeDocument  = "document"
	mediaTypeVideo     = "video"
	mediaTypeAnimation = "animation"
	mediaTypeAudio     = "audio"
	mediaTypeVoice     = "voice"
	mediaTypeOther     = "other"
)

// MediaInfo 媒体文件信息
type MediaInfo struct {
	MessageID int64  // Telegram 消息ID（TDLib 为大整数）
	TDFileID  int32  // TDLib 内部文件ID，用于 DownloadFile
	MediaType string // photo/document/video/animation/audio/voice
	FileName  string
	FileSize  int64
	MimeType  string
	ChatID    int64
	Date      time.Time
	TaskID    string // 所属任务ID（CLI 模式使用合成ID）
}

// RecordStatus 下载记录状态
type RecordStatus string

const (
	// RecordStarted 表示开始下载
	RecordStarted RecordStatus = "downloading"
	// RecordCompleted 表示下载完成
	RecordCompleted RecordStatus = "completed"
	// RecordFailed 表示下载失败
	RecordFailed RecordStatus = "failed"
	// RecordSkipped 表示跳过下载（文件已存在）
	RecordSkipped RecordStatus = "skipped"
)

// RecordEvent 下载历史记录事件
type RecordEvent struct {
	Media    *MediaInfo
	Status   RecordStatus
	FilePath string
	Reason   string
}

// Downloader 下载器
type Downloader struct {
	downloadPath   string
	maxConcurrent  int
	logger         *logger.Logger
	semaphore      chan struct{}
	stats          *DownloadStats
	downloadFunc   func(context.Context, *MediaInfo, string) error
	classifyByType bool
	recordFunc     func(context.Context, RecordEvent)
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
func New(downloadPath string, maxConcurrent int, logger *logger.Logger) *Downloader {
	return &Downloader{
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

// SetClassifyByType 设置是否按媒体类型分类存储
func (d *Downloader) SetClassifyByType(v bool) {
	d.classifyByType = v
}

// SetRecordFunc 设置下载历史记录回调
func (d *Downloader) SetRecordFunc(fn func(context.Context, RecordEvent)) {
	d.recordFunc = fn
}

// record 触发下载历史记录回调，未设置时无操作
func (d *Downloader) record(ctx context.Context, evt RecordEvent) {
	if d.recordFunc == nil {
		return
	}
	d.recordFunc(ctx, evt)
}

// classifyDir 根据媒体类型返回分类子目录名
func classifyDir(mediaType string) string {
	switch mediaType {
	case mediaTypePhoto, mediaTypeDocument, mediaTypeVideo, mediaTypeAnimation, mediaTypeAudio, mediaTypeVoice:
		return mediaType
	default:
		return mediaTypeOther
	}
}

// Stats 是下载统计的只读快照（无锁，便于 JSON 序列化）
type Stats struct {
	Total          int   `json:"total"`
	Downloaded     int   `json:"downloaded"`
	Failed         int   `json:"failed"`
	Skipped        int   `json:"skipped"`
	TotalSize      int64 `json:"total_size"`
	DownloadedSize int64 `json:"downloaded_size"`
}

// Snapshot 返回当前下载统计的只读快照
func (d *Downloader) Snapshot() Stats {
	d.stats.mu.RLock()
	defer d.stats.mu.RUnlock()
	return Stats{
		Total:          d.stats.Total,
		Downloaded:     d.stats.Downloaded,
		Failed:         d.stats.Failed,
		Skipped:        d.stats.Skipped,
		TotalSize:      d.stats.TotalSize,
		DownloadedSize: d.stats.DownloadedSize,
	}
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
	if d.classifyByType {
		chatDir = filepath.Join(chatDir, classifyDir(media.MediaType))
	}
	if err := os.MkdirAll(chatDir, DirectoryPermission); err != nil {
		d.logger.Error("创建目录失败: %v", err)
		d.updateStats(false, 0)
		d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: chatDir})
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: chatDir, Reason: err.Error()})
		return err
	}

	// 生成文件路径
	fileName := media.FileName
	if fileName == "" {
		ext := d.getFileExtension(media.MimeType)
		fileName = fmt.Sprintf("file_%d_%d%s", media.MessageID, media.TDFileID, ext)
	}

	// 清理文件名以防止路径遍历攻击
	fileName = d.sanitizeFileName(fileName)
	filePath := filepath.Join(chatDir, fileName)

	// 验证文件路径安全性
	if !d.isSafePath(filePath, d.downloadPath) {
		d.logger.Error("不安全的文件路径: %s", filePath)
		d.updateStats(false, 0)
		err := fmt.Errorf("unsafe file path: %s", filePath)
		d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: filePath})
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: err.Error()})
		return err
	}

	// 检查文件是否已存在
	if _, err := os.Stat(filePath); err == nil {
		d.logger.Debug("文件已存在，跳过下载: %s", fileName)
		d.stats.mu.Lock()
		d.stats.Skipped++
		d.stats.mu.Unlock()
		d.record(ctx, RecordEvent{Media: media, Status: RecordSkipped, FilePath: filePath})
		return nil
	}

	if d.downloadFunc == nil {
		d.updateStats(false, 0)
		d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: filePath})
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: "下载函数未设置"})
		return fmt.Errorf("下载函数未设置")
	}

	d.logger.Info("开始下载: %s (大小: %d bytes)", fileName, media.FileSize)
	d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: filePath})

	if err := d.downloadFunc(ctx, media, filePath); err != nil {
		d.logger.Error("下载失败 %s: %v", fileName, err)
		d.updateStats(false, 0)
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: err.Error()})
		return err
	}

	d.logger.Info("下载完成: %s", fileName)
	d.updateStats(true, media.FileSize)
	d.record(ctx, RecordEvent{Media: media, Status: RecordCompleted, FilePath: filePath})
	return nil
}

// sanitizeFileName 清理文件名，移除危险字符
func (d *Downloader) sanitizeFileName(fileName string) string {
	// 移除路径分隔符和其他危险字符
	fileName = strings.ReplaceAll(fileName, "/", "_")
	fileName = strings.ReplaceAll(fileName, "\\", "_")
	fileName = strings.ReplaceAll(fileName, "..", "_")
	fileName = strings.ReplaceAll(fileName, ":", "_")
	fileName = strings.ReplaceAll(fileName, "*", "_")
	fileName = strings.ReplaceAll(fileName, "?", "_")
	fileName = strings.ReplaceAll(fileName, "\"", "_")
	fileName = strings.ReplaceAll(fileName, "<", "_")
	fileName = strings.ReplaceAll(fileName, ">", "_")
	fileName = strings.ReplaceAll(fileName, "|", "_")

	// 确保文件名不为空
	if fileName == "" || fileName == "." || fileName == ".." {
		fileName = "unnamed_file"
	}

	return fileName
}

// isSafePath 验证文件路径是否安全（在指定的基础目录内）
func (d *Downloader) isSafePath(filePath, basePath string) bool {
	// 获取绝对路径
	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}

	absBasePath, err := filepath.Abs(basePath)
	if err != nil {
		return false
	}

	// 检查文件路径是否在基础路径内
	relPath, err := filepath.Rel(absBasePath, absFilePath)
	if err != nil {
		return false
	}

	// 如果相对路径包含".."，说明试图访问基础目录外的文件
	return !strings.HasPrefix(relPath, "..") && !strings.Contains(relPath, "/..")
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

// DownloadSingle 下载单个媒体文件（用于实时监控）
func (d *Downloader) DownloadSingle(ctx context.Context, media *MediaInfo) {
	d.stats.mu.Lock()
	d.stats.Total++
	d.stats.TotalSize += media.FileSize
	d.stats.mu.Unlock()

	d.logger.Info("检测到新媒体文件，开始下载: %s", media.FileName)

	if err := d.DownloadMedia(ctx, media); err != nil {
		d.logger.Error("下载新媒体文件失败: %v", err)
	}
}

// DownloadBatch 批量下载媒体文件，返回的 WaitGroup 供调用方等待本批次完成
func (d *Downloader) DownloadBatch(ctx context.Context, mediaList []*MediaInfo) *sync.WaitGroup {
	d.stats.mu.Lock()
	d.stats.Total += len(mediaList)
	for _, media := range mediaList {
		d.stats.TotalSize += media.FileSize
	}
	d.stats.mu.Unlock()

	d.logger.Info("开始批量下载 %d 个文件", len(mediaList))

	wg := &sync.WaitGroup{}
	for _, media := range mediaList {
		wg.Add(1)
		go func(m *MediaInfo) {
			defer wg.Done()
			if err := d.DownloadMedia(ctx, m); err != nil {
				d.logger.Error("下载媒体文件失败: %v", err)
			}
		}(media)
	}
	return wg
}

// PrintStats 打印下载统计
func (d *Downloader) PrintStats() {
	stats := d.Snapshot()
	d.logger.Info("下载统计:")
	d.logger.Info("  总计: %d", stats.Total)
	d.logger.Info("  已下载: %d", stats.Downloaded)
	d.logger.Info("  失败: %d", stats.Failed)
	d.logger.Info("  跳过: %d", stats.Skipped)
	d.logger.Info("  总大小: %.2f MB", float64(stats.TotalSize)/MegabyteDivisor)
	d.logger.Info("  已下载大小: %.2f MB", float64(stats.DownloadedSize)/MegabyteDivisor)
}
