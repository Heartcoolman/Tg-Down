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

	mu            sync.Mutex
	status        Status
	errMsg        string
	startedAt     *time.Time
	finishedAt    *time.Time
	stats         downloader.Stats
	cancel        context.CancelFunc
	phase         string // 运行阶段（counting/downloading），仅内存态
	expectedTotal int64  // 下载前统计出的媒体总数（近似值），0 表示未知

	scannedMessages int64     // 历史扫描已翻阅的消息数（仅内存态，运行中有值）
	foundMedia      int64     // 历史扫描累计发现的媒体数（仅内存态，运行中有值）
	lastScanNotify  time.Time // 上次扫描进度对外推送时刻，用于限频

	lastRecordNotify      time.Time // 上次下载记录对外推送时刻，用于限频
	recordTrailingPending bool      // 是否已排定一次尾随推送（限频窗口内多次更新只排一次）
}

// scanNotifyMinGap 是扫描进度对外推送（SSE）的最小间隔：
// 本地缓存命中时历史页可毫秒级连续返回，不限频会造成广播风暴
const scanNotifyMinGap = 500 * time.Millisecond

// recordNotifyMinGap 是下载记录对外推送（SSE）的最小间隔：
// 重试时的跳过风暴或大量小文件快速完成会产生每文件一次的 task 事件，
// 不限频会向浏览器灌入成百上千条消息造成前端卡顿
const recordNotifyMinGap = 250 * time.Millisecond

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
		id:            row.ID,
		kind:          Kind(row.Kind),
		chatID:        row.ChatID,
		chatTitle:     row.ChatTitle,
		createdAt:     row.CreatedAt,
		done:          make(chan struct{}),
		status:        Status(row.Status),
		errMsg:        row.Error,
		startedAt:     row.StartedAt,
		finishedAt:    row.FinishedAt,
		expectedTotal: row.ExpectedTotal,
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
		ID:              t.id,
		Kind:            string(t.kind),
		ChatID:          t.chatID,
		ChatTitle:       t.chatTitle,
		Status:          string(t.status),
		Error:           t.errMsg,
		CreatedAt:       t.createdAt,
		StartedAt:       t.startedAt,
		FinishedAt:      t.finishedAt,
		Stats:           t.stats,
		Phase:           t.phase,
		ExpectedTotal:   t.expectedTotal,
		ScannedMessages: t.scannedMessages,
		FoundMedia:      t.foundMedia,
	}
}

// applyScanProgress 更新扫描进度，返回本次是否应对外推送（按 scanNotifyMinGap 限频）
func (t *task) applyScanProgress(scannedMessages, foundMedia int64) (notify bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scannedMessages = scannedMessages
	t.foundMedia = foundMedia
	if time.Since(t.lastScanNotify) < scanNotifyMinGap {
		return false
	}
	t.lastScanNotify = time.Now()
	return true
}

// markRecordNotify 采用前沿限频 + 尾随补发：距上次推送超过 recordNotifyMinGap 立即推送；
// 否则在窗口内首次触发时请求排定一次尾随推送（返回 scheduleTrailing），确保突发结束后的最终态不丢失。
func (t *task) markRecordNotify() (now, scheduleTrailing bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.lastRecordNotify) >= recordNotifyMinGap {
		t.lastRecordNotify = time.Now()
		t.recordTrailingPending = false
		return true, false
	}
	if t.recordTrailingPending {
		return false, false
	}
	t.recordTrailingPending = true
	return false, true
}

// clearRecordTrailing 在尾随推送真正发出前复位限频状态，使后续更新能重新触发推送
func (t *task) clearRecordTrailing() {
	t.mu.Lock()
	t.recordTrailingPending = false
	t.lastRecordNotify = time.Now()
	t.mu.Unlock()
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
