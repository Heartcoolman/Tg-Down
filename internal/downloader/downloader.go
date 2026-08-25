// Package downloader provides media file downloading functionality for Tg-Down application.
// It supports concurrent downloads with progress tracking and statistics.
package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const (
	// 进度状态（MediaProgress.Status）：与 RecordStatus 语义不同，独立成组
	progressPaused      = "paused"
	progressDownloading = "downloading"

	// metadataFilePerm 限制 sidecar 仅当前用户可读；其中含 caption、发送者与聊天 ID。
	metadataFilePerm = 0o600

	// thumbsDirName 是缩略图缓存目录（位于下载根目录下）。
	// 以点开头，与 .tdlib-files 一样不会混进用户的媒体目录里。
	thumbsDirName = ".thumbs"

	// parentDir 用于判断相对路径是否越出下载根目录
	parentDir = ".."
)

// MediaInfo 媒体文件信息
type MediaInfo struct {
	MessageID int64  // Telegram 消息ID（TDLib 为大整数）
	TDFileID  int32  // TDLib 内部文件ID，用于 DownloadFile（会话本地，重启后失效）
	UniqueID  string // TDLib remote file unique_id，跨聊天/跨会话稳定，用于内容级去重
	MediaType string // photo/document/video/animation/audio/voice
	FileName  string
	FileSize  int64
	MimeType  string
	ChatID    int64
	Date      time.Time
	TaskID    string // 所属任务ID（CLI 模式使用合成ID）
	AlbumID   int64  // Telegram 相册（media_album_id），0 = 不属于相册
	Caption   string // 消息 caption 文本（供元数据 sidecar）
	SenderID  int64  // 发送者 user/chat id（供元数据 sidecar）
	ChatTitle string // 聊天标题（供路径模板的 {chat_title}），可为空

	// Minithumb 是 TDLib 随消息免费返回的极小 JPEG（约 40x40、几百字节），
	// 不需要任何额外下载。画廊用它做即时占位图。
	Minithumb []byte
	// ThumbFileID 是缩略图的 TDLib 文件 id（0 = 该媒体没有缩略图）。
	// 缩略图是独立的小文件，需单独下载一次。
	ThumbFileID int32
	// ThumbUniqueID 是缩略图的 remote unique_id，用作 .thumbs/ 下的缓存文件名
	ThumbUniqueID string
}

// RecordStatus 下载记录状态
type RecordStatus string

const (
	// RecordQueued 表示媒体已进入下载流水线（尚未必然开始传输）。
	//
	// 取值是 "queued" 而非 "downloading"：事件在抢占下载槽位之前发出，而在途媒体上限
	// （partition_size，默认 100）远大于下载并发（max_concurrent，默认 5），因此写
	// "downloading" 会让库里出现上百行"下载中"而实际只有 5 个在传输。真正的传输态由内存
	// 进度（MediaProgress）表达，不落库——落库的两次写入仍是"入队"与"终态"，写放大不变。
	RecordQueued RecordStatus = "queued"
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
	DownloadedSize int64  // 实际下载字节数（RecordCompleted 时填充，用于精确统计；0 表示未知）
	ThumbPath      string // 缩略图缓存路径（RecordCompleted 时填充；无缩略图为空）
}

// Downloader 下载器
type Downloader struct {
	downloadPath string
	logger       *logger.Logger
	limiter      *concurrencyLimiter
	stats        *DownloadStats
	downloadFunc func(context.Context, *MediaInfo, string) error
	pauseFunc    func(context.Context, *MediaInfo) error
	// thumbFunc 下载缩略图文件（与主文件下载分开：缩略图失败无关紧要，不进进度表、不重试）
	thumbFunc      func(ctx context.Context, fileID int32, destPath string) error
	classifyByType atomic.Bool  // Web 端可运行时切换，下载 goroutine 并发读取
	saveMetadata   atomic.Bool  // 下载完成后是否写元数据 sidecar
	pathTpl        atomic.Value // 落盘路径模板（string），空 = DefaultPathTemplate
	recordFunc     func(context.Context, *RecordEvent)
	// duplicateLookupFunc 按 unique_id 查找已完成下载的既有文件路径（内容级去重），可为 nil
	duplicateLookupFunc func(context.Context, string) (string, bool)

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

	// 同一路径只能有一个写入者；同一 TDLib file id 也只能有一个传输请求。
	// 这同时封住“检查存在后并发 rename”与共享 TDLib 缓存文件互相搬走的竞态。
	targetGate *keyedGate[string]
	fileGate   *keyedGate[int32]
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
		targetGate:        newKeyedGate[string](),
		fileGate:          newKeyedGate[int32](),
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

// SetThumbDownloadFunc 设置缩略图下载函数（可为 nil = 不下载缩略图）
func (d *Downloader) SetThumbDownloadFunc(fn func(ctx context.Context, fileID int32, destPath string) error) {
	d.thumbFunc = fn
}

// ThumbsDir 返回缩略图缓存目录
func (d *Downloader) ThumbsDir() string {
	return filepath.Join(d.downloadPath, thumbsDirName)
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
func (d *Downloader) SetRecordFunc(fn func(context.Context, *RecordEvent)) {
	d.recordFunc = fn
}

// SetDuplicateLookupFunc 设置内容级去重查找回调：按 TDLib remote unique_id 返回
// 已完成下载的既有文件路径（ok=false 表示无记录）。由持有 store 的一方注入。
func (d *Downloader) SetDuplicateLookupFunc(fn func(ctx context.Context, uniqueID string) (existingPath string, ok bool)) {
	d.duplicateLookupFunc = fn
}

// SetSaveMetadata 设置是否在下载完成后写 <文件>.json 元数据 sidecar
func (d *Downloader) SetSaveMetadata(v bool) {
	d.saveMetadata.Store(v)
}

// SaveMetadata 返回是否在下载完成后写元数据 sidecar
func (d *Downloader) SaveMetadata() bool {
	return d.saveMetadata.Load()
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
func (d *Downloader) record(ctx context.Context, evt *RecordEvent) {
	if d.recordFunc == nil {
		return
	}
	d.recordFunc(ctx, evt)
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
	// SpeedBps 是该文件的实时下载速度（字节/秒）。此前只有全局聚合速度，
	// 单个文件卡住时用户看不出是哪一个在拖后腿。
	SpeedBps int64 `json:"speed_bps"`
	// ETASeconds 是该文件的预计剩余秒数（速度未知或已完成时为 0）
	ETASeconds int64 `json:"eta_seconds,omitempty"`

	// 速度平滑用的内部状态（不序列化）
	lastBytes int64
	lastAt    time.Time
}

const (
	// speedEMAWeight 是新观测值在指数滑动平均中的权重（越大越灵敏、越抖动）
	speedEMAWeight = 0.3
	// speedMinInterval 是两次速度采样的最小间隔，避免过密采样把噪声放大
	speedMinInterval = 300 * time.Millisecond
)

// updateSpeed 按本次观测刷新单文件的速度与剩余时间（调用方持 progressMu 写锁）
func (p *MediaProgress) updateSpeed(downloaded int64, now time.Time) {
	if p.lastAt.IsZero() {
		p.lastBytes, p.lastAt = downloaded, now
		return
	}
	elapsed := now.Sub(p.lastAt)
	if elapsed < speedMinInterval {
		return
	}

	delta := downloaded - p.lastBytes
	p.lastBytes, p.lastAt = downloaded, now
	if delta < 0 {
		return // TDLib 重置了该文件的进度（例如恢复下载），本次不计
	}

	instant := float64(delta) / elapsed.Seconds()
	if p.SpeedBps == 0 {
		p.SpeedBps = int64(instant)
	} else {
		p.SpeedBps = int64(speedEMAWeight*instant + (1-speedEMAWeight)*float64(p.SpeedBps))
	}

	if p.SpeedBps > 0 && p.FileSize > downloaded {
		p.ETASeconds = (p.FileSize - downloaded) / p.SpeedBps
	} else {
		p.ETASeconds = 0
	}
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
		if downloaded >= 0 {
			p.updateSpeed(downloaded, now)
		}
		if completed {
			p.Status = "completed"
			p.Paused = false
			p.Percent = 100
			p.ETASeconds = 0
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
		d.markProgressStatus(key, progressPaused)
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
	p.Paused = status == progressPaused
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
	d.record(ctx, &RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: "下载已取消"})
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

	wasDownloading := d.progressStatus(id) == progressDownloading
	d.markProgressStatus(id, progressPaused)
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

// DownloadMedia 下载媒体文件。
//
// 事件契约：每个媒体恰好发出一次 RecordQueued，随后恰好发出一次终态事件
// （Completed/Failed/Skipped）。任务统计据此保持 Total = Downloaded + Failed + Skipped。
func (d *Downloader) DownloadMedia(ctx context.Context, media *MediaInfo) error {
	chatDir, fileName, filePath := d.planMediaPath(media)
	media.FileName = fileName

	// 入队事件先行，保证后续任何分支（含失败）都已有对应的 history 行
	d.record(ctx, &RecordEvent{Media: media, Status: RecordQueued, FilePath: filePath})

	// 路径规划与存在性检查本身不足以防并发覆盖：两个 goroutine 可能同时看见
	// “不存在”后一起下载并 rename。相同最终路径必须从检查到终态全程串行。
	releaseTarget, err := d.targetGate.acquire(ctx, filepath.Clean(filePath))
	if err != nil {
		d.failMedia(ctx, media, filePath, err.Error())
		return err
	}
	defer releaseTarget()

	if err := d.ensureTargetDir(ctx, media, chatDir, filePath); err != nil {
		return err
	}

	// 已下载过且文件完好 → 跳过
	if d.skipIfComplete(ctx, media, filePath) {
		return nil
	}

	if d.downloadFunc == nil {
		d.failMedia(ctx, media, filePath, "下载函数未设置")
		return fmt.Errorf("下载函数未设置")
	}

	progressKey := d.startProgress(media, filePath)

	deduped, err := d.downloadWithPauseLoop(ctx, media, filePath, progressKey)
	if err != nil {
		return err
	}
	if deduped {
		return nil // 已由去重复制完成，跳过事件在 copyFromDuplicate 中记录
	}

	actual := d.downloadedBytes(progressKey)
	if actual <= 0 {
		actual = media.FileSize
	}
	d.logger.Info("下载完成: %s", media.FileName)
	d.updateStats(true, actual)
	d.finishProgress(progressKey, media, "completed")
	d.record(ctx, &RecordEvent{
		Media:          media,
		Status:         RecordCompleted,
		FilePath:       filePath,
		DownloadedSize: actual,
		ThumbPath:      d.fetchThumb(ctx, media),
	})
	d.writeMetadataSidecar(media, filePath)
	return nil
}

// fetchThumb 下载媒体的缩略图到 .thumbs/ 缓存，返回其路径（失败或无缩略图时返回空串）。
//
// 全程 best-effort：缩略图只是画廊的加速件，它失败不该影响主文件的下载结果。
// 按 unique_id 命名，因此同一文件被转发到多个聊天时只下一次。
func (d *Downloader) fetchThumb(ctx context.Context, media *MediaInfo) string {
	if d.thumbFunc == nil || media.ThumbFileID == 0 || media.ThumbUniqueID == "" {
		return ""
	}
	dir := filepath.Join(d.downloadPath, thumbsDirName)
	path := filepath.Join(dir, media.ThumbUniqueID+".jpg")

	if err := rejectSymlinkPath(d.downloadPath, path); err != nil {
		d.logger.Debug("缩略图路径包含符号链接，已跳过: %v", err)
		return ""
	}
	if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
		return path // 已缓存（同一文件转发到多个聊天）
	}
	if err := os.MkdirAll(dir, DirectoryPermission); err != nil {
		d.logger.Debug("创建缩略图目录失败: %v", err)
		return ""
	}
	if err := rejectSymlinkPath(d.downloadPath, path); err != nil {
		d.logger.Debug("缩略图路径包含符号链接，已跳过: %v", err)
		return ""
	}
	if err := d.thumbFunc(ctx, media.ThumbFileID, path); err != nil {
		d.logger.Debug("下载缩略图失败（不影响主文件）: %v", err)
		return ""
	}
	return path
}

// ensureTargetDir 校验路径安全并创建目标目录。
// 先校验后建目录：文件名来自远端消息，必须在任何 MkdirAll 之前确认其位于下载根目录内，
// 避免在校验前于任意可写路径建目录。
func (d *Downloader) ensureTargetDir(ctx context.Context, media *MediaInfo, chatDir, filePath string) error {
	if !d.isSafePath(chatDir, d.downloadPath) || !d.isSafePath(filePath, d.downloadPath) {
		d.logger.Error("不安全的文件路径: %s", filePath)
		err := fmt.Errorf("unsafe file path: %s", filePath)
		d.failMedia(ctx, media, filePath, err.Error())
		return err
	}
	if err := rejectSymlinkPath(d.downloadPath, filePath); err != nil {
		d.logger.Error("下载路径包含符号链接: %v", err)
		d.failMedia(ctx, media, filePath, err.Error())
		return err
	}
	if err := os.MkdirAll(chatDir, DirectoryPermission); err != nil {
		d.logger.Error("创建目录失败: %v", err)
		d.failMedia(ctx, media, chatDir, err.Error())
		return err
	}
	// MkdirAll 与首次检查之间目录可能发生变化，再检查一次现有的完整路径。
	if err := rejectSymlinkPath(d.downloadPath, filePath); err != nil {
		d.logger.Error("下载路径包含符号链接: %v", err)
		d.failMedia(ctx, media, filePath, err.Error())
		return err
	}
	return nil
}

// rejectSymlinkPath 检查 base 下从第一层目录到 target 的每个已存在路径段。
// 远端名称本身不能创建符号链接，但应用不应跟随下载目录里预先放置的链接写到根目录之外。
func rejectSymlinkPath(base, target string) error {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return err
	}
	if rel == parentDir || filepath.IsAbs(rel) || strings.HasPrefix(rel, parentDir+string(filepath.Separator)) {
		return fmt.Errorf("路径位于下载根目录之外: %s", target)
	}
	if rel == "." {
		return nil
	}

	current := absBase
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
			return fmt.Errorf("路径段是符号链接: %s", current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("路径段不是目录: %s", current)
		}
	}
	return nil
}

// failMedia 记一次失败（统计 + 终态事件）
func (d *Downloader) failMedia(ctx context.Context, media *MediaInfo, filePath, reason string) {
	d.updateStats(false, 0)
	d.record(ctx, &RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: reason})
}

// skipIfComplete 判断目标文件是否已存在且完好，是则记跳过并返回 true。
//
// 仅凭 os.Stat 存在就跳过是不够的：一个 0 字节或被截断的残留文件（旧版本、外部工具或
// 手动操作留下的）会被永久跳过，用户永远拿不到这个文件。此处比对 TDLib 给出的期望大小。
func (d *Downloader) skipIfComplete(ctx context.Context, media *MediaInfo, filePath string) bool {
	info, err := os.Stat(filePath)
	if err != nil {
		return false
	}
	if info.IsDir() || info.Size() == 0 {
		d.logger.Warn("已存在路径不是完整文件，重新下载: %s", media.FileName)
		return false
	}
	if media.FileSize > 0 && info.Size() != media.FileSize {
		d.logger.Warn("已存在文件大小不符（%d != %d），重新下载: %s", info.Size(), media.FileSize, media.FileName)
		return false
	}
	d.logger.Debug("文件已存在，跳过下载: %s", media.FileName)
	d.recordSkip(ctx, media, filePath, "")
	// 文件可能来自开关关闭时的旧运行或外部工具，此时并无 sidecar；补一份使
	// "开关开启后每个受管文件都有元数据"成立。
	d.ensureMetadataSidecar(media, filePath)
	return true
}

// recordSkip 统计并记录一次跳过事件
func (d *Downloader) recordSkip(ctx context.Context, media *MediaInfo, filePath, reason string) {
	d.stats.mu.Lock()
	d.stats.Skipped++
	d.stats.mu.Unlock()
	d.record(ctx, &RecordEvent{Media: media, Status: RecordSkipped, FilePath: filePath, Reason: reason})
}

// copyFromDuplicate 尝试按 unique_id 从既有文件复制；成功返回 true（已记 skipped）
func (d *Downloader) copyFromDuplicate(ctx context.Context, media *MediaInfo, filePath string) bool {
	if media.UniqueID == "" || d.duplicateLookupFunc == nil {
		return false
	}
	src, ok := d.duplicateLookupFunc(ctx, media.UniqueID)
	if !ok || src == "" || src == filePath {
		return false
	}
	info, err := os.Stat(src)
	if err != nil {
		return false // 源文件已删，照常下载
	}
	// unique_id 只能证明 Telegram 端内容相同，不能证明本地源文件仍完好。
	// 期望大小未知时也无法完成校验，宁可重新下载，不能复制一个可能损坏的文件并记成功。
	if info.IsDir() || media.FileSize <= 0 || info.Size() != media.FileSize {
		d.logger.Warn("去重源大小无法验证或不匹配，回退为正常下载: %s", src)
		return false
	}
	if err := linkOrCopy(src, filePath); err != nil {
		d.logger.Warn("去重复制失败，回退为正常下载: %v", err)
		return false
	}
	d.logger.Info("内容重复，已从既有文件复制: %s <- %s", media.FileName, src)
	d.recordSkip(ctx, media, filePath, "duplicate of "+src)
	// 去重复制出的文件同样要有 sidecar：linkOrCopy 优先硬链接，但 .json 是按目标
	// 路径独立的文件，不会随硬链接一起出现。
	d.ensureMetadataSidecar(media, filePath)
	return true
}

// linkOrCopy 优先建硬链接，失败（跨文件系统、不支持硬链接、目标已存在）时回退为整文件复制。
//
// 硬链接让同一份内容在多个聊天/相册目录下只占一份磁盘空间——转发到 10 个群的同一个视频
// 此前会被实实在在地复制 10 份。
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// downloadWithPauseLoop 执行带暂停/恢复语义的下载循环，直至成功、失败或取消。
// deduped 为真表示文件由内容级去重复制完成，未经过实际下载（终态事件已记录）。
func (d *Downloader) downloadWithPauseLoop(
	ctx context.Context, media *MediaInfo, filePath, progressKey string,
) (deduped bool, err error) {
	for {
		if err := d.waitUntilResumed(ctx, progressKey); err != nil {
			d.finishCanceled(ctx, progressKey, media, filePath)
			return false, err
		}

		// TDLib 的下载与取消都按 file_id 操作。相同 file_id 若同时进入两个任务，
		// 会共享缓存路径并互相取消，因此每次真实尝试必须按 file_id 串行。
		releaseFile := func() {}
		if media.TDFileID != 0 {
			var err error
			releaseFile, err = d.fileGate.acquire(ctx, media.TDFileID)
			if err != nil {
				d.finishCanceled(ctx, progressKey, media, filePath)
				return false, err
			}
		}
		if d.isMediaPaused(progressKey) {
			releaseFile()
			d.markProgressStatus(progressKey, progressPaused)
			continue
		}
		if err := d.limiter.acquire(ctx); err != nil {
			releaseFile()
			d.finishCanceled(ctx, progressKey, media, filePath)
			return false, err
		}
		// acquire 可能阻塞较久，其间可能收到 PauseMedia；重新检查暂停状态，
		// 若已暂停则释放槽位回到循环等待恢复，避免暂停被 "downloading" 覆盖后静默下载完成。
		if d.isMediaPaused(progressKey) {
			d.limiter.release()
			releaseFile()
			d.markProgressStatus(progressKey, progressPaused)
			continue
		}

		// 内容级去重在持槽之后执行：它要查一次库、还可能整文件复制，都是实打实的 IO。
		// 放在槽外时，在途上限（partition_size，默认 100）会让上百个 goroutine 同时做
		// 全文件复制并争抢 4 条 SQLite 连接，而 max_concurrent（默认 5）完全管不到它们。
		if d.copyFromDuplicate(ctx, media, filePath) {
			d.limiter.release()
			releaseFile()
			d.finishProgress(progressKey, media, "skipped")
			return true, nil
		}

		d.markProgressStatus(progressKey, progressDownloading)
		d.beginAttempt(progressKey)
		d.logger.Info("开始下载: %s (大小: %d bytes)", media.FileName, media.FileSize)
		dlErr := d.downloadFunc(ctx, media, filePath)
		d.limiter.release()
		releaseFile()
		if dlErr == nil {
			return false, nil
		}
		// 暂停诱发的取消不算失败：用 pauseRequestedSince 判定（而非当前 paused 状态），
		// 以覆盖“暂停后立即恢复”导致 paused 已被清除、错误却仍是暂停取消的竞态。
		if d.isMediaPaused(progressKey) || d.pauseRequestedSince(progressKey) {
			d.logger.Info("已暂停下载: %s", media.FileName)
			d.markProgressStatus(progressKey, progressPaused)
			continue
		}
		d.logger.Error("下载失败 %s: %v", media.FileName, dlErr)
		d.updateStats(false, 0)
		d.finishProgress(progressKey, media, "failed")
		d.record(ctx, &RecordEvent{Media: media, Status: RecordFailed, FilePath: filePath, Reason: dlErr.Error()})
		return false, dlErr
	}
}

// mediaSidecar 是 <文件>.json 元数据的载荷结构
type mediaSidecar struct {
	MessageID  int64  `json:"message_id"`
	ChatID     int64  `json:"chat_id"`
	ChatTitle  string `json:"chat_title,omitempty"`
	Date       int64  `json:"date"`
	DateText   string `json:"date_text,omitempty"`
	SenderID   int64  `json:"sender_id"`
	Caption    string `json:"caption"`
	AlbumID    int64  `json:"album_id"`
	MediaType  string `json:"media_type"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size"`
	MimeType   string `json:"mime_type"`
	UniqueID   string `json:"unique_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	MessageURL string `json:"message_url,omitempty"`
}

// ensureMetadataSidecar 在 sidecar 尚不存在时补写一份。
//
// 用于文件并非由本次下载产生、但确实在此路径落了盘的两条路径：内容去重复制，
// 以及目标文件已存在而跳过。它们此前都在 writeMetadataSidecar 的调用点之前返回，
// 于是同一份内容先在哪个聊天下载，决定了另一处有没有 sidecar。
// 已存在则不覆盖：那份是当初下载时按其自身消息写的，比这次的更贴切。
func (d *Downloader) ensureMetadataSidecar(media *MediaInfo, filePath string) {
	if !d.saveMetadata.Load() {
		return
	}
	if _, err := os.Stat(filePath + ".json"); err == nil {
		return
	}
	d.writeMetadataSidecar(media, filePath)
}

// writeMetadataSidecar 在开关开启时写 <文件>.json 元数据（best-effort，失败仅告警）
func (d *Downloader) writeMetadataSidecar(media *MediaInfo, filePath string) {
	if !d.saveMetadata.Load() {
		return
	}
	payload := mediaSidecar{
		MessageID:  media.MessageID,
		ChatID:     media.ChatID,
		ChatTitle:  media.ChatTitle,
		Date:       media.Date.Unix(),
		DateText:   sidecarDateText(media.Date),
		SenderID:   media.SenderID,
		Caption:    media.Caption,
		AlbumID:    media.AlbumID,
		MediaType:  media.MediaType,
		FileName:   media.FileName,
		FileSize:   media.FileSize,
		MimeType:   media.MimeType,
		UniqueID:   media.UniqueID,
		TaskID:     media.TaskID,
		MessageURL: messageURL(media.ChatID, media.MessageID),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		d.logger.Warn("序列化元数据失败: %v", err)
		return
	}
	if err := writeFileAtomic(filePath+".json", data, metadataFilePerm); err != nil {
		d.logger.Warn("写入元数据 sidecar 失败: %v", err)
	}
}

// sidecarDateText 返回 sidecar 中的人类可读消息日期；零值时间为空串
func sidecarDateText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// messageURL 拼出可直接打开该消息的 t.me 链接。
// 私有群/频道统一用 c/<chatID>/<msgID> 形式（chat_id 负数变正数）。
func messageURL(chatID, messageID int64) string {
	if chatID == 0 || messageID == 0 {
		return ""
	}
	if chatID < 0 {
		chatID = -chatID
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", chatID, messageID)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := true
	defer func() {
		_ = tmp.Close()
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	remove = false
	return nil
}

// copyFile 将 src 复制为 dst：先写入同目录临时文件再原子 rename，
// 避免复制中途崩溃留下半截文件被后续 skip-if-exists 误判为已完成
func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- src 来自本应用写入的下载历史记录
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
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

// planMediaPath 按路径模板算出媒体的落盘目录与完整路径。
//
// 越界兜底：模板由用户配置，展开的值又来自 Telegram（文件名、聊天标题都是他人可控的），
// 因此结果必须落回下载根目录之内，否则拒绝该模板结果、退回默认布局。
func (d *Downloader) planMediaPath(media *MediaInfo) (chatDir, fileName, filePath string) {
	ctx := pathContext{media: media, classifyByType: d.classifyByType.Load()}

	rel := expandPathTemplate(d.pathTemplate(), ctx)
	filePath = filepath.Join(d.downloadPath, rel)
	if !d.isSafePath(filePath, d.downloadPath) {
		rel = expandPathTemplate(DefaultPathTemplate, ctx)
		filePath = filepath.Join(d.downloadPath, rel)
	}

	chatDir = filepath.Dir(filePath)
	fileName = filepath.Base(filePath)
	return chatDir, fileName, filePath
}

// TargetPath 返回该媒体按当前模板应落盘的完整路径（不触发下载）。
// 供导出功能定位已下载的文件。
func (d *Downloader) TargetPath(media *MediaInfo) string {
	_, _, filePath := d.planMediaPath(media)
	return filePath
}

// DownloadPath 返回下载根目录
func (d *Downloader) DownloadPath() string { return d.downloadPath }

// PathTemplate 返回当前生效的路径模板（未配置时为默认布局）
func (d *Downloader) PathTemplate() string { return d.pathTemplate() }

// pathTemplate 返回当前生效的路径模板（未配置时为默认布局）
func (d *Downloader) pathTemplate() string {
	tpl, _ := d.pathTpl.Load().(string)
	if tpl == "" {
		return DefaultPathTemplate
	}
	return tpl
}

// SetPathTemplate 设置落盘路径模板；非法模板被忽略并退回默认布局，
// 避免一个手滑的配置把所有文件写到下载根目录之外
func (d *Downloader) SetPathTemplate(tpl string) {
	if tpl == "" || ValidatePathTemplate(tpl) != "" {
		d.pathTpl.Store(DefaultPathTemplate)
		return
	}
	d.pathTpl.Store(tpl)
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

	return relPath != parentDir && !filepath.IsAbs(relPath) &&
		!strings.HasPrefix(relPath, parentDir+string(filepath.Separator))
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
