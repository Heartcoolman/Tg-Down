package store

import "time"

// TaskRow 表示 tasks 表中的一行下载任务记录
type TaskRow struct {
	ID             string
	Kind           string
	ChatID         int64
	ChatTitle      string
	Status         string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	Error          string
	Total          int
	Downloaded     int
	Failed         int
	Skipped        int
	TotalSize      int64
	DownloadedSize int64
	ExpectedTotal  int64
}

// 任务状态常量
const (
	TaskStatusPending   = "pending"
	TaskStatusRunning   = "running"
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
	TaskStatusCanceled  = "canceled"
)

// HistoryRecord 表示 history 表中的一行下载历史记录
type HistoryRecord struct {
	ID         int64
	TaskID     string
	ChatID     int64
	ChatTitle  string
	MessageID  int64
	MediaType  string
	FileName   string
	FilePath   string
	FileSize   int64
	MimeType   string
	Status     string
	Reason     string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// 下载历史状态常量，取值与 downloader.RecordStatus 保持一致
const (
	HistoryStatusDownloading = "downloading"
	HistoryStatusCompleted   = "completed"
	HistoryStatusFailed      = "failed"
	HistoryStatusSkipped     = "skipped"
)

// HistoryFilter 描述 QueryHistory/HistoryStats 的过滤与分页条件
type HistoryFilter struct {
	MediaType string
	Status    string
	Query     string // 按文件名子串匹配
	ChatID    int64  // 0 表示不限制
	From, To  *time.Time
	Page      int
	PageSize  int
}

// MediaTypeStat 按媒体类型聚合的下载统计
type MediaTypeStat struct {
	MediaType string
	Count     int
	TotalSize int64
	Completed int
	Failed    int
	Skipped   int
}
