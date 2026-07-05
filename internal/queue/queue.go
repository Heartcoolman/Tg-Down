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

// ChatDownloader 是 Manager 依赖的最小接口（而非直接依赖 *telegram.Client），
// 使本包可在无真实 TDLib 连接的情况下进行单元测试。
type ChatDownloader interface {
	DownloadHistoryMedia(ctx context.Context, chatID int64, taskID string) error
	SetMonitorTask(taskID string, chatID int64)
	SetRecordFunc(fn func(context.Context, downloader.RecordEvent))
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
}
