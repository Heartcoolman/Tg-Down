// Package downloader provides media file downloading functionality for Tg-Down application.
// It supports concurrent downloads with progress tracking and statistics.
package downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tg-down/internal/logger"
)

const (
	// DirectoryPermission is the permission mode for creating directories
	DirectoryPermission = 0750
	// MegabyteDivisor is the divisor for converting bytes to megabytes
	MegabyteDivisor = 1024 * 1024
	// defaultMaxConcurrent is used when an invalid concurrency value is provided.
	defaultMaxConcurrent = 1
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
	Media          *MediaInfo
	Status         RecordStatus
	FilePath       string
	Reason         string
	DownloadedSize int64 // 实际下载字节数（RecordCompleted 时填充，用于精确统计；0 表示未知）
}

// Downloader 下载器
type Downloader struct {
	downloadPath   string
	logger         *logger.Logger
	limiter        *concurrencyLimiter
	stats          *DownloadStats
	downloadFunc   func(context.Context, *MediaInfo, string) error
	pauseFunc      func(context.Context, *MediaInfo) error
	classifyByType atomic.Bool // Web 端可运行时切换，下载 goroutine 并发读取
	recordFunc     func(context.Context, RecordEvent)

	progressMu        sync.RWMutex
	progressByKey     map[string]*MediaProgress
	progressKeyByFile map[int32]map[string]struct{} // 同一 TDLib file id 可对应多个并发下载键
	controlMu         sync.Mutex
	controls          map[string]*mediaControl
	allPaused         bool // controlMu 保护：全局暂停闸，置位后新注册媒体以暂停态开始

	rateMu      sync.Mutex
	rateLast    map[int32]int64 // TDLib file id -> 上次观测的已下载字节数（按文件去重，避免多键扇出重复计数）
	rateCum     int64           // 累计观测下载字节
	rateSamples []rateSample    // (时刻, 累计字节) 采样，按时间递增
}

type rateSample struct {
	at    time.Time
	bytes int64
}

const (
	// speedWindow 是下载速度滑动窗口长度
	speedWindow = 5 * time.Second
	// speedSampleMinGap 是相邻速率采样的最小间隔，限制采样密度
	speedSampleMinGap = 200 * time.Millisecond
)

type mediaControl struct {
	mu             sync.Mutex
	cond           *sync.Cond
	paused         bool
	pauseRequested bool // 本次下载尝试期间是否调用过 pauseFunc（用于区分暂停诱发的取消错误与真实失败）
	done           bool
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
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	return &Downloader{
		downloadPath:      downloadPath,
		logger:            logger,
		limiter:           newConcurrencyLimiter(maxConcurrent),
		stats:             &DownloadStats{},
		progressByKey:     make(map[string]*MediaProgress),
		progressKeyByFile: make(map[int32]map[string]struct{}),
		controls:          make(map[string]*mediaControl),
		rateLast:          make(map[int32]int64),
	}
}

type concurrencyLimiter struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	active int
}

func newConcurrencyLimiter(limit int) *concurrencyLimiter {
	if limit <= 0 {
		limit = defaultMaxConcurrent
	}
	l := &concurrencyLimiter{limit: limit}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *concurrencyLimiter) acquire(ctx context.Context) error {
	l.mu.Lock()
	if ctx.Err() != nil {
		l.mu.Unlock()
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.cond.Broadcast()
			l.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	for l.active >= l.limit {
		if ctx.Err() != nil {
			l.mu.Unlock()
			return ctx.Err()
		}
		l.cond.Wait()
	}
	l.active++
	l.mu.Unlock()
	return nil
}

func (l *concurrencyLimiter) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *concurrencyLimiter) setLimit(limit int) {
	if limit <= 0 {
		limit = defaultMaxConcurrent
	}
	l.mu.Lock()
	l.limit = limit
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *concurrencyLimiter) snapshot() (limit, active int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit, l.active
}

// SetDownloadFunc 设置下载函数
func (d *Downloader) SetDownloadFunc(fn func(context.Context, *MediaInfo, string) error) {
	d.downloadFunc = fn
}

// SetPauseFunc 设置底层下载暂停函数。该函数应尽快让正在下载的媒体返回，
// DownloadMedia 会把暂停视为可恢复状态，而不是失败。
func (d *Downloader) SetPauseFunc(fn func(context.Context, *MediaInfo) error) {
	d.pauseFunc = fn
}

// SetClassifyByType 设置是否按媒体类型分类存储
func (d *Downloader) SetClassifyByType(v bool) {
	d.classifyByType.Store(v)
}

// ClassifyByType 返回是否按媒体类型分类存储
func (d *Downloader) ClassifyByType() bool {
	return d.classifyByType.Load()
}

// SetRecordFunc 设置下载历史记录回调
func (d *Downloader) SetRecordFunc(fn func(context.Context, RecordEvent)) {
	d.recordFunc = fn
}

// SetMaxConcurrent updates the number of media files that may download at once.
func (d *Downloader) SetMaxConcurrent(maxConcurrent int) {
	d.limiter.setLimit(maxConcurrent)
}

// MaxConcurrent returns the current media download concurrency limit.
func (d *Downloader) MaxConcurrent() int {
	limit, _ := d.limiter.snapshot()
	return limit
}

// ActiveCount returns how many media downloads currently hold a download slot.
func (d *Downloader) ActiveCount() int {
	_, active := d.limiter.snapshot()
	return active
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

// MediaProgress describes one queued or active media download.
type MediaProgress struct {
	ID             string    `json:"id"`
	TaskID         string    `json:"task_id,omitempty"`
	MessageID      int64     `json:"message_id"`
	TDFileID       int32     `json:"td_file_id"`
	ChatID         int64     `json:"chat_id"`
	MediaType      string    `json:"media_type"`
	FileName       string    `json:"file_name"`
	FileSize       int64     `json:"file_size"`
	DownloadedSize int64     `json:"downloaded_size"`
	Percent        float64   `json:"percent"`
	Status         string    `json:"status"`
	Paused         bool      `json:"paused"`
	FilePath       string    `json:"file_path,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
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

// ActiveMedia returns queued/downloading media snapshots, sorted by start time.
func (d *Downloader) ActiveMedia() []MediaProgress {
	d.progressMu.RLock()
	defer d.progressMu.RUnlock()
	items := make([]MediaProgress, 0, len(d.progressByKey))
	for _, p := range d.progressByKey {
		items = append(items, *p)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].StartedAt.Before(items[j].StartedAt)
	})
	return items
}

// UpdateProgress updates byte-level progress for a TDLib file update. A single
// TDLib file id may back several concurrent downloads (same file forwarded to
// multiple chats/tasks), so every tracked key for that file is updated.
func (d *Downloader) UpdateProgress(tdFileID int32, downloaded, total int64, completed bool) {
	d.progressMu.Lock()
	defer d.progressMu.Unlock()
	keys := d.progressKeyByFile[tdFileID]
	if len(keys) == 0 {
		return
	}
	now := time.Now()
	d.noteFileBytes(tdFileID, downloaded, now)
	for key := range keys {
		p := d.progressByKey[key]
		if p == nil {
			continue
		}
		if total > 0 {
			p.FileSize = total
		}
		if downloaded >= 0 {
			p.DownloadedSize = downloaded
		}
		if p.FileSize > 0 {
			p.Percent = float64(p.DownloadedSize) / float64(p.FileSize) * 100
			if p.Percent > 100 {
				p.Percent = 100
			}
		}
		if completed {
			p.Status = "completed"
			p.Paused = false
			p.Percent = 100
			if p.FileSize > 0 {
				p.DownloadedSize = p.FileSize
			}
		}
		p.UpdatedAt = now
	}
}

// noteFileBytes 按 TDLib 文件维度累计下载字节正增量并采样，用于计算聚合下载速度。
// downloaded 变小视为重新下载，只重置基线不计负增量。
func (d *Downloader) noteFileBytes(fileID int32, downloaded int64, now time.Time) {
	if downloaded < 0 {
		return
	}
	d.rateMu.Lock()
	defer d.rateMu.Unlock()
	if last := d.rateLast[fileID]; downloaded > last {
		d.rateCum += downloaded - last
	}
	d.rateLast[fileID] = downloaded
	if n := len(d.rateSamples); n == 0 || now.Sub(d.rateSamples[n-1].at) >= speedSampleMinGap {
		d.rateSamples = append(d.rateSamples, rateSample{at: now, bytes: d.rateCum})
	}
	d.pruneSamplesLocked(now)
}

func (d *Downloader) pruneSamplesLocked(now time.Time) {
	cut := 0
	for cut < len(d.rateSamples) && now.Sub(d.rateSamples[cut].at) > speedWindow {
		cut++
	}
	d.rateSamples = d.rateSamples[cut:]
}

// SpeedBps 返回滑动窗口内的平均下载速度（字节/秒），无近期数据时为 0。
func (d *Downloader) SpeedBps() int64 { return d.speedAt(time.Now()) }

func (d *Downloader) speedAt(now time.Time) int64 {
	d.rateMu.Lock()
	defer d.rateMu.Unlock()
	d.pruneSamplesLocked(now)
	if len(d.rateSamples) == 0 {
		return 0
	}
	oldest := d.rateSamples[0]
	elapsed := now.Sub(oldest.at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return int64(float64(d.rateCum-oldest.bytes) / elapsed)
}

func (d *Downloader) startProgress(media *MediaInfo, filePath string) string {
	now := time.Now()
	key := mediaProgressKey(media)
	p := &MediaProgress{
		ID:        key,
		TaskID:    media.TaskID,
		MessageID: media.MessageID,
		TDFileID:  media.TDFileID,
		ChatID:    media.ChatID,
		MediaType: media.MediaType,
		FileName:  media.FileName,
		FileSize:  media.FileSize,
		Status:    "queued",
		FilePath:  filePath,
		StartedAt: now,
		UpdatedAt: now,
	}
	d.progressMu.Lock()
	d.progressByKey[key] = p
	if media.TDFileID != 0 {
		set := d.progressKeyByFile[media.TDFileID]
		if set == nil {
			set = make(map[string]struct{})
			d.progressKeyByFile[media.TDFileID] = set
		}
		set[key] = struct{}{}
	}
	d.progressMu.Unlock()
	d.controlMu.Lock()
	ctrl := &mediaControl{paused: d.allPaused}
	ctrl.cond = sync.NewCond(&ctrl.mu)
	d.controls[key] = ctrl
	paused := d.allPaused
	d.controlMu.Unlock()
	if paused {
		d.markProgressStatus(key, "paused")
	}
	return key
}

func (d *Downloader) markProgressStatus(key, status string) {
	d.progressMu.Lock()
	defer d.progressMu.Unlock()
	p := d.progressByKey[key]
	if p == nil {
		return
	}
	p.Status = status
	p.Paused = status == "paused"
	p.UpdatedAt = time.Now()
}

func (d *Downloader) finishProgress(key string, media *MediaInfo, status string) {
	d.progressMu.Lock()
	defer d.progressMu.Unlock()
	if p := d.progressByKey[key]; p != nil {
		p.Status = status
		p.UpdatedAt = time.Now()
	}
	delete(d.progressByKey, key)
	if media != nil && media.TDFileID != 0 {
		if set := d.progressKeyByFile[media.TDFileID]; set != nil {
			delete(set, key)
			if len(set) == 0 {
				delete(d.progressKeyByFile, media.TDFileID)
				d.rateMu.Lock()
				delete(d.rateLast, media.TDFileID)
				d.rateMu.Unlock()
			}
		}
	}
	d.controlMu.Lock()
	ctrl := d.controls[key]
	delete(d.controls, key)
	d.controlMu.Unlock()
	if ctrl != nil {
		ctrl.mu.Lock()
		ctrl.done = true
		ctrl.cond.Broadcast()
		ctrl.mu.Unlock()
	}
}

// finishCanceled 终结一个在等待槽位/恢复期间被取消的下载：清理进度、计入失败统计，
// 并发出终态 RecordFailed（原因标注为取消），使 history 行不会永久停留在 "downloading"，
// 且任务统计满足 Total = Downloaded + Failed + Skipped。
func (d *Downloader) finishCanceled(ctx context.Context, key string, media *MediaInfo, filePath string) {
	d.finishProgress(key, media, "canceled")
	d.updateStats(false, 0)
	d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: "下载已取消"})
}

func mediaProgressKey(media *MediaInfo) string {
	if media == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d:%d", media.TaskID, media.ChatID, media.MessageID, media.TDFileID)
}

// PauseMedia 暂停单个正在排队或下载中的媒体。若媒体正在由底层下载函数执行，
// 会调用 pauseFunc 让底层请求尽快返回；DownloadMedia 会保留该媒体并等待继续。
func (d *Downloader) PauseMedia(ctx context.Context, id string) error {
	ctrl := d.mediaControl(id)
	if ctrl == nil {
		return fmt.Errorf("媒体不存在或已结束: %s", id)
	}
	ctrl.mu.Lock()
	if ctrl.done {
		ctrl.mu.Unlock()
		return fmt.Errorf("媒体不存在或已结束: %s", id)
	}
	ctrl.paused = true
	ctrl.pauseRequested = true
	ctrl.cond.Broadcast()
	ctrl.mu.Unlock()

	wasDownloading := d.progressStatus(id) == "downloading"
	d.markProgressStatus(id, "paused")
	if d.pauseFunc != nil && wasDownloading {
		media := d.mediaByProgressID(id)
		if media != nil {
			return d.pauseFunc(ctx, media)
		}
	}
	return nil
}

// ResumeMedia 继续单个已暂停的媒体。
func (d *Downloader) ResumeMedia(id string) error {
	ctrl := d.mediaControl(id)
	if ctrl == nil {
		return fmt.Errorf("媒体不存在或已结束: %s", id)
	}
	ctrl.mu.Lock()
	if ctrl.done {
		ctrl.mu.Unlock()
		return fmt.Errorf("媒体不存在或已结束: %s", id)
	}
	ctrl.paused = false
	ctrl.cond.Broadcast()
	ctrl.mu.Unlock()
	d.markProgressStatus(id, "queued")
	return nil
}

// PauseAll 暂停全部排队中/下载中的媒体，并让此后新入队的媒体以暂停态开始。
func (d *Downloader) PauseAll(ctx context.Context) {
	d.controlMu.Lock()
	d.allPaused = true
	ids := make([]string, 0, len(d.controls))
	for id := range d.controls {
		ids = append(ids, id)
	}
	d.controlMu.Unlock()
	for _, id := range ids {
		if err := d.PauseMedia(ctx, id); err != nil {
			d.logger.Debug("暂停媒体失败（可能已结束）: %v", err)
		}
	}
}

// ResumeAll 解除全局暂停闸并继续全部已暂停的媒体。
func (d *Downloader) ResumeAll() {
	d.controlMu.Lock()
	d.allPaused = false
	ids := make([]string, 0, len(d.controls))
	for id := range d.controls {
		ids = append(ids, id)
	}
	d.controlMu.Unlock()
	for _, id := range ids {
		_ = d.ResumeMedia(id)
	}
}

// AllPaused 返回全局暂停闸状态。
func (d *Downloader) AllPaused() bool {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	return d.allPaused
}

func (d *Downloader) mediaControl(id string) *mediaControl {
	d.controlMu.Lock()
	defer d.controlMu.Unlock()
	return d.controls[id]
}

func (d *Downloader) mediaByProgressID(id string) *MediaInfo {
	d.progressMu.RLock()
	defer d.progressMu.RUnlock()
	p := d.progressByKey[id]
	if p == nil {
		return nil
	}
	return &MediaInfo{
		MessageID: p.MessageID,
		TDFileID:  p.TDFileID,
		MediaType: p.MediaType,
		FileName:  p.FileName,
		FileSize:  p.FileSize,
		ChatID:    p.ChatID,
		TaskID:    p.TaskID,
	}
}

func (d *Downloader) progressStatus(id string) string {
	d.progressMu.RLock()
	defer d.progressMu.RUnlock()
	if p := d.progressByKey[id]; p != nil {
		return p.Status
	}
	return ""
}

func (d *Downloader) waitUntilResumed(ctx context.Context, key string) error {
	ctrl := d.mediaControl(key)
	if ctrl == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			ctrl.mu.Lock()
			ctrl.cond.Broadcast()
			ctrl.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	for ctrl.paused && !ctrl.done {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ctrl.cond.Wait()
	}
	return ctx.Err()
}

func (d *Downloader) isMediaPaused(key string) bool {
	ctrl := d.mediaControl(key)
	if ctrl == nil {
		return false
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	return ctrl.paused && !ctrl.done
}

// beginAttempt 在即将调用 downloadFunc 前清除暂停请求标记，标定一次新的下载尝试。
func (d *Downloader) beginAttempt(key string) {
	if ctrl := d.mediaControl(key); ctrl != nil {
		ctrl.mu.Lock()
		ctrl.pauseRequested = false
		ctrl.mu.Unlock()
	}
}

// pauseRequestedSince 报告自上次 beginAttempt 以来是否发生过暂停请求；用于把暂停诱发的
// downloadFunc 取消错误正确识别为暂停（可恢复）而非真实失败，即便用户已快速点击恢复。
func (d *Downloader) pauseRequestedSince(key string) bool {
	ctrl := d.mediaControl(key)
	if ctrl == nil {
		return false
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	return ctrl.pauseRequested && !ctrl.done
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
	chatDir, fileName, filePath := d.planMediaPath(media)
	media.FileName = fileName

	// 先做路径安全校验，再创建目录：文件名来自远端消息，
	// 必须在任何 MkdirAll 之前确认其位于下载根目录内，避免在校验前于任意可写路径建目录。
	if !d.isSafePath(chatDir, d.downloadPath) || !d.isSafePath(filePath, d.downloadPath) {
		d.logger.Error("不安全的文件路径: %s", filePath)
		d.updateStats(false, 0)
		err := fmt.Errorf("unsafe file path: %s", filePath)
		d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: filePath})
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: err.Error()})
		return err
	}

	if err := os.MkdirAll(chatDir, DirectoryPermission); err != nil {
		d.logger.Error("创建目录失败: %v", err)
		d.updateStats(false, 0)
		d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: chatDir})
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: chatDir, Reason: err.Error()})
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

	progressKey := d.startProgress(media, filePath)
	d.record(ctx, RecordEvent{Media: media, Status: RecordStarted, FilePath: filePath})

	for {
		if err := d.waitUntilResumed(ctx, progressKey); err != nil {
			d.finishCanceled(ctx, progressKey, media, filePath)
			return err
		}
		if err := d.limiter.acquire(ctx); err != nil {
			d.finishCanceled(ctx, progressKey, media, filePath)
			return err
		}
		// acquire 可能阻塞较久，其间可能收到 PauseMedia；重新检查暂停状态，
		// 若已暂停则释放槽位回到循环等待恢复，避免暂停被 "downloading" 覆盖后静默下载完成。
		if d.isMediaPaused(progressKey) {
			d.limiter.release()
			d.markProgressStatus(progressKey, "paused")
			continue
		}
		d.markProgressStatus(progressKey, "downloading")
		d.beginAttempt(progressKey)
		d.logger.Info("开始下载: %s (大小: %d bytes)", fileName, media.FileSize)
		err := d.downloadFunc(ctx, media, filePath)
		d.limiter.release()
		if err == nil {
			break
		}
		// 暂停诱发的取消不算失败：用 pauseRequestedSince 判定（而非当前 paused 状态），
		// 以覆盖“暂停后立即恢复”导致 paused 已被清除、错误却仍是暂停取消的竞态。
		if d.isMediaPaused(progressKey) || d.pauseRequestedSince(progressKey) {
			d.logger.Info("已暂停下载: %s", fileName)
			d.markProgressStatus(progressKey, "paused")
			continue
		}
		d.logger.Error("下载失败 %s: %v", fileName, err)
		d.updateStats(false, 0)
		d.finishProgress(progressKey, media, "failed")
		d.record(ctx, RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: err.Error()})
		return err
	}

	actual := d.downloadedBytes(progressKey)
	if actual <= 0 {
		actual = media.FileSize
	}
	d.logger.Info("下载完成: %s", fileName)
	d.updateStats(true, actual)
	d.finishProgress(progressKey, media, "completed")
	d.record(ctx, RecordEvent{Media: media, Status: RecordCompleted, FilePath: filePath, DownloadedSize: actual})
	return nil
}

// downloadedBytes 读取指定进度键已下载的真实字节数（来自 TDLib updateFile），未知返回 0。
func (d *Downloader) downloadedBytes(key string) int64 {
	d.progressMu.RLock()
	defer d.progressMu.RUnlock()
	if p := d.progressByKey[key]; p != nil {
		return p.DownloadedSize
	}
	return 0
}

func (d *Downloader) planMediaPath(media *MediaInfo) (chatDir, fileName, filePath string) {
	chatDir = filepath.Join(d.downloadPath, fmt.Sprintf("chat_%d", media.ChatID))
	if d.classifyByType.Load() {
		chatDir = filepath.Join(chatDir, classifyDir(media.MediaType))
	}

	fileName = media.FileName
	if fileName == "" {
		ext := d.getFileExtension(media.MimeType)
		fileName = fmt.Sprintf("file_%d_%d%s", media.MessageID, media.TDFileID, ext)
	}
	fileName = d.sanitizeFileName(fileName)
	filePath = filepath.Join(chatDir, fileName)
	return chatDir, fileName, filePath
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

// PlanBatch 将一批已发现的媒体计入统计（Total/TotalSize）；实际下载由调用方逐个触发
func (d *Downloader) PlanBatch(mediaList []*MediaInfo) {
	if len(mediaList) == 0 {
		return
	}
	d.stats.mu.Lock()
	d.stats.Total += len(mediaList)
	for _, media := range mediaList {
		d.stats.TotalSize += media.FileSize
	}
	d.stats.mu.Unlock()
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
