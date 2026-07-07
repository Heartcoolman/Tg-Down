package queue

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/store"
)

// idSeq 为同一纳秒内的并发 Enqueue 调用提供唯一性兜底
var idSeq atomic.Int64

// generateID 生成任务 ID，纯标准库实现，无需引入新依赖
func generateID() string {
	return fmt.Sprintf("t%d%d", time.Now().UnixNano(), idSeq.Add(1))
}

// task 是队列内部的任务状态容器，自带锁保护可变字段，
// 使执行任务的 goroutine（写者）与 List/Get（读者）之间不发生数据竞争。
type task struct {
	id        string
	kind      Kind
	chatID    int64
	chatTitle string
	createdAt time.Time
	done      chan struct{} // 任务终结时关闭（经 markDone），monitor 切换时用于等待旧任务停止
	closeOnce sync.Once     // 保证 done 只被关闭一次：排队取消与执行方退出可能竞争同一任务的终结路径

	mu         sync.Mutex
	status     Status
	errMsg     string
	startedAt  *time.Time
	finishedAt *time.Time
	stats      downloader.Stats
	cancel     context.CancelFunc
}

// newTask 创建一个初始状态为 queued 的任务
func newTask(kind Kind, chatID int64, chatTitle string) *task {
	return &task{
		id:        generateID(),
		kind:      kind,
		chatID:    chatID,
		chatTitle: chatTitle,
		createdAt: time.Now(),
		done:      make(chan struct{}),
		status:    StatusQueued,
	}
}

// markDone 关闭 done 通道，多次调用安全；由任务终结的唯一执行路径调用
func (t *task) markDone() {
	t.closeOnce.Do(func() { close(t.done) })
}

// taskFromRow 是 ToDTO 的逆操作，从持久化行重建进程重启后的内存任务；
// done/closeOnce 属于进程本地状态，无法持久化，此处总是全新初始化
func taskFromRow(row *store.TaskRow) *task {
	return &task{
		id:         row.ID,
		kind:       Kind(row.Kind),
		chatID:     row.ChatID,
		chatTitle:  row.ChatTitle,
		createdAt:  row.CreatedAt,
		done:       make(chan struct{}),
		status:     Status(row.Status),
		errMsg:     row.Error,
		startedAt:  row.StartedAt,
		finishedAt: row.FinishedAt,
		stats: downloader.Stats{
			Total:          row.Total,
			Downloaded:     row.Downloaded,
			Failed:         row.Failed,
			Skipped:        row.Skipped,
			TotalSize:      row.TotalSize,
			DownloadedSize: row.DownloadedSize,
		},
	}
}

// ToDTO 加锁返回任务状态的值拷贝快照
func (t *task) ToDTO() TaskDTO {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TaskDTO{
		ID:         t.id,
		Kind:       string(t.kind),
		ChatID:     t.chatID,
		ChatTitle:  t.chatTitle,
		Status:     string(t.status),
		Error:      t.errMsg,
		CreatedAt:  t.createdAt,
		StartedAt:  t.startedAt,
		FinishedAt: t.finishedAt,
		Stats:      t.stats,
	}
}

// applyRecordEvent 按下载事件更新任务统计，增量规则与 downloader.DownloadStats 保持一致：
// RecordStarted/RecordSkipped 各计一次 Total（互斥触发，不会重复计数同一媒体项）。
func (t *task) applyRecordEvent(evt downloader.RecordEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch evt.Status {
	case downloader.RecordStarted:
		t.stats.Total++
	case downloader.RecordSkipped:
		t.stats.Total++
		t.stats.Skipped++
	case downloader.RecordCompleted:
		t.stats.Downloaded++
		if evt.DownloadedSize > 0 {
			t.stats.DownloadedSize += evt.DownloadedSize
		} else {
			t.stats.DownloadedSize += evt.Media.FileSize
		}
	case downloader.RecordFailed:
		t.stats.Failed++
	}
}
