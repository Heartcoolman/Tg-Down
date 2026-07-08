// Package queue 实现位于 internal/telegram、internal/downloader、internal/store 之上的任务队列管理器：
// history 任务经有界 worker 池调度（受 maxConcurrentTasks 限制），monitor 任务长期运行、不占用该配额。
package queue

import (
	"context"
	"time"

	"tg-down/internal/downloader"
)

// Kind 任务类型
type Kind string

// 任务类型常量
const (
	// KindHistory 历史媒体下载任务，短生命周期，受 maxConcurrentTasks 并发限制
	KindHistory Kind = "history"
	// KindMonitor 实时监控任务，长生命周期，独立运行不占用并发配额
	KindMonitor Kind = "monitor"
)

// Status 任务状态
type Status string

// 任务状态常量
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// history 任务运行阶段常量（仅内存态，不落库；重启后 running 任务从持久化游标恢复续跑）
const (
	phaseCounting    = "counting"
	phaseDownloading = "downloading"
)

// ChatDownloader 是 Manager 依赖的最小接口（而非直接依赖 *telegram.Client），
// 使本包可在无真实 TDLib 连接的情况下进行单元测试。
type ChatDownloader interface {
	CountHistoryMedia(ctx context.Context, chatID int64, mediaTypes []string) (int64, error)
	DownloadHistoryMedia(ctx context.Context, spec *downloader.HistorySpec) error
	SetMonitorTask(taskID string, chatID int64)
	SetRecordFunc(fn func(context.Context, downloader.RecordEvent))
	SetScanProgressFunc(fn func(taskID string, scannedMessages, foundMedia, scanCursor int64))
	SetDuplicateLookupFunc(fn func(ctx context.Context, uniqueID string) (existingPath string, ok bool))
}

// TaskDTO 是任务状态对外暴露的值拷贝快照，用于 List/Get/onChange，不持有内部指针
type TaskDTO struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	ChatID     int64            `json:"chat_id"`
	ChatTitle  string           `json:"chat_title,omitempty"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	StartedAt  *time.Time       `json:"started_at,omitempty"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
	Stats      downloader.Stats `json:"stats"`
	// Phase 是 history 任务的运行阶段（counting/downloading），仅运行中有值
	Phase string `json:"phase,omitempty"`
	// ExpectedTotal 是下载前统计出的媒体总数（近似值），0 表示未知
	ExpectedTotal int64 `json:"expected_total,omitempty"`
	// ScannedMessages/FoundMedia 是 history 任务扫描历史的实时进度（仅运行中有值，不落库）
	ScannedMessages int64 `json:"scanned_messages,omitempty"`
	FoundMedia      int64 `json:"found_media,omitempty"`
	// ScanCursor 是持久化的历史扫描游标（最后已扫描页的最旧 message_id），重启恢复时续扫起点
	ScanCursor int64 `json:"scan_cursor,omitempty"`
	// Attempts 是自动重试已消耗的次数
	Attempts int `json:"attempts,omitempty"`
	// Filters 是任务级过滤条件（nil = 不过滤）
	Filters *downloader.HistoryFilters `json:"filters,omitempty"`
	// MessageID 非 0 时为单消息下载任务（t.me 消息链接）
	MessageID int64 `json:"message_id,omitempty"`
}
