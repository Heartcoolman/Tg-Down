package queue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/store"
)

const (
	// historyQueueBuffer 是 history 任务等待队列的缓冲区大小，超出后 Enqueue 阻塞直至有空位
	historyQueueBuffer = 256
	// recordQueueBuffer 是下载记录异步持久化队列的缓冲区大小，满载时丢弃并告警（见 handleRecordEvent）
	recordQueueBuffer = 256
	// recordDrainTimeout 是 Manager 关闭时清空 recordCh 剩余积压的最长等待时间
	recordDrainTimeout = 5 * time.Second
	// persistInterval 是运行中任务进度周期性落盘的间隔，避免崩溃/硬杀丢失中途进度统计
	persistInterval = 10 * time.Second
	// retryBaseBackoff/retryMaxBackoff 界定任务级自动重试的指数退避区间
	retryBaseBackoff = 30 * time.Second
	retryMaxBackoff  = 5 * time.Minute
)

// Manager 任务队列管理器：history 任务经有界 worker 池调度，monitor 任务独立运行，互不阻塞。
type Manager struct {
	client             ChatDownloader
	store              *store.Store
	logger             *logger.Logger
	maxConcurrentTasks int
	autoRetry          int // 任务级自动重试上限（0 = 关闭）
	recorder           func(context.Context, downloader.RecordEvent)
	retryBackoff       func(attempt int) time.Duration // 可注入以便测试

	historyCh chan *task
	recordCh  chan downloader.RecordEvent // 下载记录持久化的异步队列，见 handleRecordEvent/recordWriter

	// resumeHistory/resumeMonitor 由 loadTasks 收集、Run 启动时消费一次：
	// 进程重启前排队中/运行中的任务在此恢复续跑，而非回收为 failed
	resumeHistory []*task
	resumeMonitor *task

	mu          sync.Mutex
	tasks       map[string]*task
	order       []*task // 插入顺序（最早在前），List() 据此反转为最新优先
	monitorTask *task
	onChange    func(*TaskDTO)
	onTerminal  func(TaskDTO) // 任务终结通知（completed/最终 failed，取消与自动重试不触发）
	runCtx      context.Context

	monitorMu sync.Mutex // 串行化 monitor 切换，保证同一时刻至多一个 monitor 任务在运行
}

// NewManager 创建任务队列管理器：将 client 的下载记录/去重回调指向自身，
// 并从 store 恢复既有任务列表（重启前排队中/运行中的任务标记待恢复，Run 启动时续跑）
func NewManager(client ChatDownloader, st *store.Store, log *logger.Logger, maxConcurrentTasks, autoRetry int) *Manager {
	if maxConcurrentTasks <= 0 {
		maxConcurrentTasks = 1
	}
	if autoRetry < 0 {
		autoRetry = 0
	}
	m := &Manager{
		client:             client,
		store:              st,
		logger:             log,
		maxConcurrentTasks: maxConcurrentTasks,
		autoRetry:          autoRetry,
		recorder:           store.NewRecorder(st),
		retryBackoff:       defaultRetryBackoff,
		historyCh:          make(chan *task, historyQueueBuffer),
		recordCh:           make(chan downloader.RecordEvent, recordQueueBuffer),
		tasks:              make(map[string]*task),
	}
	client.SetRecordFunc(m.handleRecordEvent)
	client.SetScanProgressFunc(m.handleScanProgress)
	client.SetDuplicateLookupFunc(func(ctx context.Context, uniqueID string) (string, bool) {
		rec, err := st.FindCompletedByUniqueID(ctx, uniqueID)
		if err != nil || rec == nil {
			return "", false
		}
		return rec.FilePath, true
	})
	m.loadTasks(context.Background())
	return m
}

// defaultRetryBackoff 计算第 attempt 次自动重试前的指数退避时长
func defaultRetryBackoff(attempt int) time.Duration {
	d := retryBaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= retryMaxBackoff {
			return retryMaxBackoff
		}
	}
	return d
}

// handleScanProgress 是 client 的历史扫描进度回调：更新任务内存态（含持久化游标）并限频推送任务变更事件，
// 使前端在扫描阶段（媒体队列可能为空）也能看到任务仍在推进
func (m *Manager) handleScanProgress(taskID string, scannedMessages, foundMedia, scanCursor int64) {
	m.mu.Lock()
	t := m.tasks[taskID]
	m.mu.Unlock()
	if t == nil {
		return
	}
	if t.applyScanProgress(scannedMessages, foundMedia, scanCursor) {
		m.notify(t)
	}
}

// loadTasks 从 store 恢复任务历史列表，供 NewManager 在接受任何新任务前调用一次：
// 终态任务原样载入；重启前排队中/运行中的 history 任务重置为 queued 并记入待恢复列表
// （保留统计/游标，Run 启动后从游标续扫并补下中断行）；运行中的 monitor 任务同样待恢复重启。
func (m *Manager) loadTasks(ctx context.Context) {
	// 终结上次运行遗留的 "downloading" 历史行（原因 interrupted，恢复时据此补下），
	// 避免其永久滞留污染统计/筛选
	if n, err := m.store.SweepInterruptedHistory(ctx); err != nil {
		m.logger.Warn("清理中断的下载历史失败: %v", err)
	} else if n > 0 {
		m.logger.Info("已将 %d 条中断的下载历史标记为待补下", n)
	}

	rows, err := m.store.ListTasks(ctx)
	if err != nil {
		m.logger.Warn("恢复任务历史失败: %v", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// ListTasks 按创建时间倒序（最新在前）返回，m.order 需保持最早在前，故逆序插入
	for i := len(rows) - 1; i >= 0; i-- {
		t := taskFromRow(rows[i])
		switch {
		case t.kind == KindHistory && (t.status == StatusQueued || t.status == StatusRunning):
			t.status = StatusQueued
			t.startedAt = nil
			t.resumed = true
			if err := m.store.UpdateTaskStatus(ctx, t.id, string(t.status), ""); err != nil {
				m.logger.Warn("持久化任务恢复状态失败: %v", err)
			}
			m.resumeHistory = append(m.resumeHistory, t)
			m.logger.Info("任务 %s（聊天 %d）待恢复：游标 %d", t.id, t.chatID, t.scanCursor)
		case t.kind == KindMonitor && t.status == StatusRunning:
			// 监控任务重启后自动恢复（用户开着的监控预期保持开启），Run 启动时重建 goroutine
			if m.resumeMonitor == nil {
				m.resumeMonitor = t
			} else {
				// 数据异常：多个 running monitor，只恢复最新的一个，其余终结
				t.status = StatusCanceled
				now := time.Now()
				t.finishedAt = &now
				t.markDone()
				if err := m.store.UpdateTaskStatus(ctx, t.id, string(t.status), ""); err != nil {
					m.logger.Warn("持久化任务恢复状态失败: %v", err)
				}
			}
		default:
			t.markDone() // 终态任务不会再有 goroutine 为其运行
		}
		m.tasks[t.id] = t
		m.order = append(m.order, t)
	}
}

// SetOnChange 设置任务生命周期变化回调（created/running/completed/failed/canceled），不逐文件触发
func (m *Manager) SetOnChange(fn func(*TaskDTO)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// SetOnTerminal 设置任务终结回调：completed 与自动重试耗尽后的最终 failed 触发，
// canceled 与重试中的中间失败不触发；按任务粒度调用
func (m *Manager) SetOnTerminal(fn func(TaskDTO)) {
	m.mu.Lock()
	m.onTerminal = fn
	m.mu.Unlock()
}

// fireTerminal 触发任务终结回调（若已注册）
func (m *Manager) fireTerminal(t *task) {
	m.mu.Lock()
	fn := m.onTerminal
	m.mu.Unlock()
	if fn != nil {
		fn(t.ToDTO())
	}
}

// Run 启动 history worker 池与记录持久化 writer，阻塞直至 ctx 取消；取消后停止接受新任务执行，
// 所有运行中任务的 ctx 均派生自 ctx，会随之自动取消。启动时一次性消费 loadTasks 收集的待恢复任务。
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	resumeHistory := m.resumeHistory
	resumeMonitor := m.resumeMonitor
	m.resumeHistory = nil
	m.resumeMonitor = nil
	m.mu.Unlock()

	recordStop := make(chan struct{})
	recordDone := make(chan struct{})
	go func() {
		defer close(recordDone)
		m.recordWriter(recordStop)
	}()

	go m.persistLoop(ctx)
	go m.runScheduler(ctx)

	var wg sync.WaitGroup
	for i := 0; i < m.maxConcurrentTasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.historyWorker(ctx)
		}()
	}

	if resumeMonitor != nil {
		m.restartMonitor(ctx, resumeMonitor)
	}
	for _, t := range resumeHistory {
		m.notify(t)
		select {
		case m.historyCh <- t:
		case <-ctx.Done():
		}
	}

	<-ctx.Done()
	// 先等所有 history worker 退出（不再产生记录事件），再让 recordWriter 清空剩余积压，
	// 避免 drain 在生产者仍在发事件时因通道瞬时为空而提前退出，丢失关停时的终态记录。
	wg.Wait()
	close(recordStop)
	<-recordDone
}

// recordWriter 是唯一的下载记录消费者：按接收顺序（FIFO）串行调用 recorder 完成持久化，
// 单一生产者-消费者顺序天然保证同一 taskID 的 Started 先于 Completed/Failed 落盘；
// stop 关闭（由 Run 在所有 worker 退出后触发）时转入 drainRecordCh 清空剩余积压
func (m *Manager) recordWriter(stop <-chan struct{}) {
	for {
		select {
		case evt := <-m.recordCh:
			m.recorder(context.Background(), evt)
		case <-stop:
			m.drainRecordCh()
			return
		}
	}
}

// drainRecordCh 在 Manager 关闭时尽力清空 recordCh 中已缓冲的事件，最多等待 recordDrainTimeout；
// 不保证覆盖硬杀进程时的最后在途事件——这是已知且可接受的取舍，与 store/recorder.go
// “历史记录不得阻塞/影响下载”的既定原则一致
func (m *Manager) drainRecordCh() {
	deadline := time.NewTimer(recordDrainTimeout)
	defer deadline.Stop()
	for {
		select {
		case evt := <-m.recordCh:
			m.recorder(context.Background(), evt)
		case <-deadline.C:
			return
		default:
			return
		}
	}
}

// persistLoop 周期性将运行中任务的进度统计落盘，使崩溃/硬杀后恢复的计数接近最新，
// 并让长期运行的 monitor 任务不再仅在停止时才持久化统计。
func (m *Manager) persistLoop(ctx context.Context) {
	ticker := time.NewTicker(persistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.persistRunning()
		case <-ctx.Done():
			m.persistRunning() // 关停前再落盘一次
			return
		}
	}
}

// persistRunning 快照当前处于 running 状态的任务并逐个落盘（不在持有 m.mu 时执行 DB 写入）
func (m *Manager) persistRunning() {
	m.mu.Lock()
	running := make([]*task, 0, len(m.tasks))
	for _, t := range m.tasks {
		t.mu.Lock()
		st := t.status
		t.mu.Unlock()
		if st == StatusRunning {
			running = append(running, t)
		}
	}
	m.mu.Unlock()
	for _, t := range running {
		m.persist(t)
	}
}

// historyWorker 是 maxConcurrentTasks 个并发 worker 之一，从 history 队列串行取任务执行
func (m *Manager) historyWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-m.historyCh:
			if !ok {
				return
			}
			m.runHistoryTask(ctx, t)
		}
	}
}

// runHistoryTask 执行单个 history 任务的完整生命周期：queued -> running -> completed/failed/canceled
func (m *Manager) runHistoryTask(ctx context.Context, t *task) {
	t.mu.Lock()
	if t.status != StatusQueued {
		t.mu.Unlock()
		t.markDone() // 排队期间已被 Cancel，早退路径也需保证 done 被关闭
		return
	}
	taskCtx, cancel := context.WithCancel(ctx)
	t.status = StatusRunning
	now := time.Now()
	t.startedAt = &now
	t.cancel = cancel
	t.mu.Unlock()

	m.persist(t)
	m.notify(t)

	t.mu.Lock()
	isSingleMessage := t.messageID != 0
	mediaTypes := t.filters.MediaTypes
	t.mu.Unlock()

	// 计数阶段：下载开始前先统计媒体总数并落库+推送，前端立即可见"共约 N 个"；
	// 单消息任务无需统计，总数恒为 1
	t.mu.Lock()
	t.phase = phaseCounting
	t.mu.Unlock()
	m.notify(t)
	if isSingleMessage {
		t.mu.Lock()
		t.expectedTotal = 1
		t.mu.Unlock()
		m.persist(t)
	} else if total, cntErr := m.client.CountHistoryMedia(taskCtx, t.chatID, mediaTypes); cntErr != nil {
		if taskCtx.Err() == nil {
			m.logger.Warn("统计任务 %s 媒体总数失败，回退为未知总数: %v", t.id, cntErr)
		}
	} else {
		m.logger.Info("聊天 %d 共约 %d 个媒体文件", t.chatID, total)
		t.mu.Lock()
		t.expectedTotal = total
		t.mu.Unlock()
		m.persist(t)
	}
	t.mu.Lock()
	t.phase = phaseDownloading
	spec := downloader.HistorySpec{
		ChatID:        t.chatID,
		TaskID:        t.id,
		FromMessageID: t.scanCursor,
		MessageID:     t.messageID,
		Filters:       t.filters,
	}
	resumed := t.resumed
	t.mu.Unlock()
	m.notify(t)

	// 恢复的任务先补下被进程重启清扫的中断行：这些消息比游标更新，仅靠游标续扫会永久漏掉
	if resumed {
		if ids, listErr := m.store.ListInterruptedByTask(taskCtx, t.id); listErr != nil {
			m.logger.Warn("查询任务 %s 中断行失败: %v", t.id, listErr)
		} else if len(ids) > 0 {
			m.logger.Info("任务 %s 恢复：补下 %d 个中断的媒体", t.id, len(ids))
			spec.RetryMessageIDs = ids
		}
	}

	err := m.client.DownloadHistoryMedia(taskCtx, spec)

	t.mu.Lock()
	t.cancel = nil
	t.phase = ""
	t.scannedMessages = 0
	t.foundMedia = 0
	t.resumed = false
	canceled := taskCtx.Err() != nil
	retryScheduled := false
	if err == nil {
		t.status = StatusCompleted
		t.scanCursor = 0 // 完整扫完，清游标；后续手动重试从头重扫（去重使重扫廉价）
	} else if !canceled && m.autoRetry > 0 && t.attempts < m.autoRetry {
		// 自动重试：同一任务 id 续命（保留游标/统计），退避后重新入队
		t.attempts++
		t.status = StatusQueued
		t.errMsg = err.Error()
		t.resumed = true // 重跑前补下本轮中断的行
		retryScheduled = true
	} else {
		finishedAt := time.Now()
		t.finishedAt = &finishedAt
		if canceled {
			t.status = StatusCanceled
		} else {
			// final-failure point：自动重试耗尽的最终失败（M4 完成通知在此触发）
			t.status = StatusFailed
			t.errMsg = err.Error()
		}
	}
	attempt := t.attempts
	t.mu.Unlock()
	cancel()

	m.persist(t)
	m.notify(t)
	if retryScheduled {
		m.scheduleRetry(t, attempt, err)
		return // 任务未终结，不 markDone
	}
	if !canceled {
		m.fireTerminal(t) // completed 或最终 failed
	}
	t.markDone()
}

// scheduleRetry 在指数退避后把任务重新投入 history 队列；触发时若任务已被取消或管理器已关停则放弃
func (m *Manager) scheduleRetry(t *task, attempt int, cause error) {
	backoff := m.retryBackoff(attempt)
	m.logger.Warn("任务 %s 失败（%v），%s 后自动重试（第 %d/%d 次）", t.id, cause, backoff, attempt, m.autoRetry)
	time.AfterFunc(backoff, func() {
		m.mu.Lock()
		runCtx := m.runCtx
		m.mu.Unlock()
		if runCtx == nil || runCtx.Err() != nil {
			return
		}
		t.mu.Lock()
		stillQueued := t.status == StatusQueued
		t.mu.Unlock()
		if !stillQueued { // 退避期间被用户取消
			return
		}
		select {
		case m.historyCh <- t:
		case <-runCtx.Done():
		}
	})
}

// Enqueue 创建并提交一个新任务。history 任务进入有界 worker 池排队；
// monitor 任务立即以独立 goroutine 长期运行（不占用 history 配额），ChatID 为 0 表示停止监控。
// spec 携带 ChatID 以及 history 任务的过滤器/单消息参数（monitor 忽略后两者）。
func (m *Manager) Enqueue(kind Kind, spec downloader.HistorySpec, chatTitle string) (TaskDTO, error) {
	switch kind {
	case KindHistory:
		return m.enqueueHistory(spec, chatTitle)
	case KindMonitor:
		return m.enqueueMonitor(spec.ChatID, chatTitle)
	default:
		return TaskDTO{}, fmt.Errorf("未知任务类型: %s", kind)
	}
}

// enqueueHistory 创建 history 任务、持久化后投递给 worker 池；
// 排队中/运行中的重复任务拒绝创建（整聊天任务按 chatID 去重，单消息任务按 (chatID, messageID) 去重）
func (m *Manager) enqueueHistory(spec downloader.HistorySpec, chatTitle string) (TaskDTO, error) {
	m.mu.Lock()
	for _, existing := range m.tasks {
		if existing.kind != KindHistory || existing.chatID != spec.ChatID {
			continue
		}
		existing.mu.Lock()
		status := existing.status
		existingMsgID := existing.messageID
		existing.mu.Unlock()
		if status != StatusQueued && status != StatusRunning {
			continue
		}
		if existingMsgID != spec.MessageID {
			continue // 单消息任务与整聊天任务互不冲突，不同消息的单消息任务亦然
		}
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("该会话已有下载任务在队列中")
	}

	t := newTask(KindHistory, spec, chatTitle)
	if err := m.createTaskRow(t); err != nil {
		m.mu.Unlock()
		return TaskDTO{}, err
	}
	m.tasks[t.id] = t
	m.order = append(m.order, t)
	m.mu.Unlock()

	m.notify(t)
	m.historyCh <- t
	return t.ToDTO(), nil
}

// enqueueMonitor 取消当前 monitor 任务（若有）并等待其退出，再视 chatID 决定是否启动新任务；
// chatID == 0 表示仅停止监控：返回被取消任务的快照（无任务时返回零值 TaskDTO），不创建新任务。
func (m *Manager) enqueueMonitor(chatID int64, chatTitle string) (TaskDTO, error) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()

	m.mu.Lock()
	prev := m.monitorTask
	runCtx := m.runCtx
	m.mu.Unlock()

	if prev != nil {
		_ = m.cancelTask(prev)
		<-prev.done
	}

	if chatID == 0 {
		if prev != nil {
			return prev.ToDTO(), nil
		}
		return TaskDTO{}, nil
	}

	t := newTask(KindMonitor, downloader.HistorySpec{ChatID: chatID}, chatTitle)
	t.mu.Lock()
	t.status = StatusRunning
	now := time.Now()
	t.startedAt = &now
	t.mu.Unlock()

	if err := m.createTaskRow(t); err != nil {
		return TaskDTO{}, err
	}

	if runCtx == nil {
		runCtx = context.Background()
	}
	taskCtx, cancel := context.WithCancel(runCtx)
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()

	m.mu.Lock()
	m.tasks[t.id] = t
	m.order = append(m.order, t)
	m.monitorTask = t
	m.mu.Unlock()

	// 同步建立 client 端关联，确保调用方一旦观察到任务状态为 running，
	// SetMonitorTask 必然已经生效，不会有 goroutine 异步设置带来的可见性竞争。
	m.client.SetMonitorTask(t.id, t.chatID)
	m.notify(t)

	go m.runMonitorTask(taskCtx, t)
	return t.ToDTO(), nil
}

// restartMonitor 重启进程重启前仍在运行的 monitor 任务：沿用同一任务 id 重建 ctx 与 client 端关联
func (m *Manager) restartMonitor(runCtx context.Context, t *task) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()

	taskCtx, cancel := context.WithCancel(runCtx)
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()

	m.mu.Lock()
	m.monitorTask = t
	m.mu.Unlock()

	m.client.SetMonitorTask(t.id, t.chatID)
	m.logger.Info("已恢复监控任务 %s（聊天 %d）", t.id, t.chatID)
	m.notify(t)
	go m.runMonitorTask(taskCtx, t)
}

// runMonitorTask 阻塞至 ctx 取消，结束时清理 client 端关联并转为 canceled
func (m *Manager) runMonitorTask(ctx context.Context, t *task) {
	defer t.markDone()
	<-ctx.Done()
	m.client.SetMonitorTask("", 0)

	t.mu.Lock()
	t.cancel = nil
	t.status = StatusCanceled
	now := time.Now()
	t.finishedAt = &now
	t.mu.Unlock()

	m.mu.Lock()
	if m.monitorTask == t {
		m.monitorTask = nil
	}
	m.mu.Unlock()

	m.persist(t)
	m.notify(t)
}

// List 返回全部任务快照，按创建时间倒序（最新优先）；返回值始终为拷贝，不暴露内部指针
func (m *Manager) List() []TaskDTO {
	m.mu.Lock()
	ts := make([]*task, len(m.order))
	copy(ts, m.order)
	m.mu.Unlock()

	dtos := make([]TaskDTO, len(ts))
	for i, t := range ts {
		dtos[len(ts)-1-i] = t.ToDTO()
	}
	return dtos
}

// Get 按 ID 查询单个任务快照
func (m *Manager) Get(id string) (TaskDTO, bool) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return TaskDTO{}, false
	}
	return t.ToDTO(), true
}

// Cancel 取消一个排队中或运行中的任务：排队中的任务直接标记为 canceled；
// 运行中的任务通过取消其 ctx 触发执行方退出，最终状态由执行方自行落定（runHistoryTask/runMonitorTask）。
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	return m.cancelTask(t)
}

// cancelTask 按任务当前状态执行取消，仅排队/运行中的任务可取消
func (m *Manager) cancelTask(t *task) error {
	t.mu.Lock()
	status := t.status
	if status == StatusQueued {
		t.status = StatusCanceled
		now := time.Now()
		t.finishedAt = &now
		t.mu.Unlock()
		t.markDone() // 排队中即被取消，任务永不会被 worker 执行，需在此终结 done
		m.persist(t)
		m.notify(t)
		return nil
	}
	if status == StatusRunning {
		cancel := t.cancel
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	t.mu.Unlock()
	return fmt.Errorf("任务状态为 %s，无法取消", status)
}

// Retry 重新提交一个失败/取消的任务：保留 kind/chatID/chatTitle，以新 ID 重新入队
// （不复用旧 ID/旧记录，旧任务行原样保留在历史列表中）。
func (m *Manager) Retry(id string) (TaskDTO, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return TaskDTO{}, fmt.Errorf("任务不存在: %s", id)
	}

	t.mu.Lock()
	status, kind, chatTitle := t.status, t.kind, t.chatTitle
	spec := downloader.HistorySpec{ChatID: t.chatID, Filters: t.filters, MessageID: t.messageID}
	t.mu.Unlock()

	if status != StatusFailed && status != StatusCanceled {
		return TaskDTO{}, fmt.Errorf("任务状态为 %s，不允许重试", status)
	}
	return m.Enqueue(kind, spec, chatTitle)
}

// createTaskRow 按任务当前快照在 store 中创建持久化记录
func (m *Manager) createTaskRow(t *task) error {
	dto := t.ToDTO()
	row := &store.TaskRow{
		ID:            dto.ID,
		Kind:          dto.Kind,
		ChatID:        dto.ChatID,
		ChatTitle:     dto.ChatTitle,
		Status:        dto.Status,
		CreatedAt:     dto.CreatedAt,
		StartedAt:     dto.StartedAt,
		ExpectedTotal: dto.ExpectedTotal,
		ScanCursor:    dto.ScanCursor,
		Attempts:      dto.Attempts,
		Filters:       t.filtersJSON(),
		MessageID:     dto.MessageID,
	}
	if err := m.store.CreateTask(context.Background(), row); err != nil {
		return fmt.Errorf("创建任务记录失败: %w", err)
	}
	return nil
}

// persist 将任务当前状态与统计快照写入 store；写入失败仅记录日志，不影响内存中的任务状态
func (m *Manager) persist(t *task) {
	dto := t.ToDTO()
	ctx := context.Background()
	if err := m.store.UpdateTaskStatus(ctx, dto.ID, dto.Status, dto.Error); err != nil {
		m.logger.Warn("持久化任务状态失败: %v", err)
	}
	if err := m.store.UpdateTaskProgress(ctx, dto.ID, store.TaskProgress{
		Total: dto.Stats.Total, Downloaded: dto.Stats.Downloaded,
		Failed: dto.Stats.Failed, Skipped: dto.Stats.Skipped,
		TotalSize: dto.Stats.TotalSize, DownloadedSize: dto.Stats.DownloadedSize,
		ExpectedTotal: dto.ExpectedTotal, ScanCursor: dto.ScanCursor, Attempts: dto.Attempts,
	}); err != nil {
		m.logger.Warn("持久化任务进度失败: %v", err)
	}
}

// notify 在任务生命周期变化时调用 onChange 回调（不持有锁执行，避免回调重入造成死锁）
func (m *Manager) notify(t *task) {
	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		dto := t.ToDTO()
		fn(&dto)
	}
}

// handleRecordEvent 是注册给 client 的下载记录回调，运行在下载 goroutine（持有下载并发信号量）上，
// 因此拆成两部分：内存 Stats 更新（廉价的互斥自增）在此同步完成；store 持久化写入则投递给
// recordCh，交由 recordWriter 异步串行处理，避免下载并发度被本地 DB 写入延迟拖慢。
func (m *Manager) handleRecordEvent(_ context.Context, evt downloader.RecordEvent) {
	if evt.Media != nil {
		m.mu.Lock()
		t := m.tasks[evt.Media.TaskID]
		m.mu.Unlock()
		if t != nil {
			t.applyRecordEvent(evt)
			if now, trailing := t.markRecordNotify(); now {
				m.notify(t)
			} else if trailing {
				// 尾随补发：限频窗口结束后发一次，携带此刻最新的累计统计
				time.AfterFunc(recordNotifyMinGap, func() {
					t.clearRecordTrailing()
					m.notify(t)
				})
			}
		}
	}
	select {
	case m.recordCh <- evt:
	default:
		// 队列已满：改为同步落盘而非丢弃，避免丢失 Started 事件后其 Completed 更新命中 0 行
		// 导致“已下载文件无任何历史记录”的静默数据丢失。仅在突发过载时短暂阻塞下载 goroutine。
		m.logger.Warn("下载记录持久化队列已满，改为同步写入: status=%s", evt.Status)
		m.recorder(context.Background(), evt)
	}
}
