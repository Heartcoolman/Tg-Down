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
	// interruptedTaskErrMsg 是重启后无法恢复的排队/运行中任务被回收为 failed 时写入的错误信息
	interruptedTaskErrMsg = "进程重启，任务中断"
)

// Manager 任务队列管理器：history 任务经有界 worker 池调度，monitor 任务独立运行，互不阻塞。
type Manager struct {
	client             ChatDownloader
	store              *store.Store
	logger             *logger.Logger
	maxConcurrentTasks int
	recorder           func(context.Context, downloader.RecordEvent)

	historyCh chan *task
	recordCh  chan downloader.RecordEvent // 下载记录持久化的异步队列，见 handleRecordEvent/recordWriter

	mu          sync.Mutex
	tasks       map[string]*task
	order       []*task // 插入顺序（最早在前），List() 据此反转为最新优先
	monitorTask *task
	onChange    func(*TaskDTO)
	runCtx      context.Context

	monitorMu sync.Mutex // 串行化 monitor 切换，保证同一时刻至多一个 monitor 任务在运行
}

// NewManager 创建任务队列管理器：将 client 的下载记录回调指向自身（用于同步任务级 Stats 并异步持久化历史），
// 并从 store 恢复既有任务列表（进程重启后的排队中/运行中任务因不再有对应 goroutine，一律回收为 failed）
func NewManager(client ChatDownloader, st *store.Store, log *logger.Logger, maxConcurrentTasks int) *Manager {
	if maxConcurrentTasks <= 0 {
		maxConcurrentTasks = 1
	}
	m := &Manager{
		client:             client,
		store:              st,
		logger:             log,
		maxConcurrentTasks: maxConcurrentTasks,
		recorder:           store.NewRecorder(st),
		historyCh:          make(chan *task, historyQueueBuffer),
		recordCh:           make(chan downloader.RecordEvent, recordQueueBuffer),
		tasks:              make(map[string]*task),
	}
	client.SetRecordFunc(m.handleRecordEvent)
	m.loadTasks(context.Background())
	return m
}

// loadTasks 从 store 恢复任务历史列表，供 NewManager 在接受任何新任务前调用一次：
// 终态任务原样载入；排队中/运行中的任务因进程重启后不再有对应 goroutine 而无法恢复，
// 一律回收为 failed 并同步持久化，确保 store 与内存状态一致后再对外可见（List/Get）
func (m *Manager) loadTasks(ctx context.Context) {
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
		if t.status == StatusQueued || t.status == StatusRunning {
			t.status = StatusFailed
			t.errMsg = interruptedTaskErrMsg
			now := time.Now()
			t.finishedAt = &now
			if err := m.store.UpdateTaskStatus(ctx, t.id, string(t.status), t.errMsg); err != nil {
				m.logger.Warn("持久化任务重启回收状态失败: %v", err)
			}
		}
		t.markDone() // 载入的任务均已处于终态，不会再有 goroutine 为其运行
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

// Run 启动 history worker 池与记录持久化 writer，阻塞直至 ctx 取消；取消后停止接受新任务执行，
// 所有运行中任务的 ctx 均派生自 ctx，会随之自动取消。
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()

	recordDone := make(chan struct{})
	go func() {
		defer close(recordDone)
		m.recordWriter(ctx)
	}()

	var wg sync.WaitGroup
	for i := 0; i < m.maxConcurrentTasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.historyWorker(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	<-recordDone
}

// recordWriter 是唯一的下载记录消费者：按接收顺序（FIFO）串行调用 recorder 完成持久化，
// 单一生产者-消费者顺序天然保证同一 taskID 的 Started 先于 Completed/Failed 落盘；
// ctx 取消后转入 drainRecordCh 做有限时间的尽力清空
func (m *Manager) recordWriter(ctx context.Context) {
	for {
		select {
		case evt := <-m.recordCh:
			m.recorder(context.Background(), evt)
		case <-ctx.Done():
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

	err := m.client.DownloadHistoryMedia(taskCtx, t.chatID, t.id)

	t.mu.Lock()
	t.cancel = nil
	finishedAt := time.Now()
	t.finishedAt = &finishedAt
	switch {
	case err == nil:
		t.status = StatusCompleted
	case taskCtx.Err() != nil:
		t.status = StatusCanceled
	default:
		t.status = StatusFailed
		t.errMsg = err.Error()
	}
	t.mu.Unlock()
	cancel()

	m.persist(t)
	m.notify(t)
	t.markDone()
}

// Enqueue 创建并提交一个新任务。history 任务进入有界 worker 池排队；
// monitor 任务立即以独立 goroutine 长期运行（不占用 history 配额），chatID 为 0 表示停止监控。
func (m *Manager) Enqueue(kind Kind, chatID int64, chatTitle string) (TaskDTO, error) {
	switch kind {
	case KindHistory:
		return m.enqueueHistory(chatID, chatTitle)
	case KindMonitor:
		return m.enqueueMonitor(chatID, chatTitle)
	default:
		return TaskDTO{}, fmt.Errorf("未知任务类型: %s", kind)
	}
}

// enqueueHistory 创建 history 任务、持久化后投递给 worker 池；
// 同一 chatID 已存在排队中/运行中的 history 任务时拒绝创建，避免重复下载
func (m *Manager) enqueueHistory(chatID int64, chatTitle string) (TaskDTO, error) {
	m.mu.Lock()
	for _, existing := range m.tasks {
		if existing.kind != KindHistory || existing.chatID != chatID {
			continue
		}
		existing.mu.Lock()
		status := existing.status
		existing.mu.Unlock()
		if status == StatusQueued || status == StatusRunning {
			m.mu.Unlock()
			return TaskDTO{}, fmt.Errorf("该会话已有下载任务在队列中")
		}
	}

	t := newTask(KindHistory, chatID, chatTitle)
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

	t := newTask(KindMonitor, chatID, chatTitle)
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
	status, kind, chatID, chatTitle := t.status, t.kind, t.chatID, t.chatTitle
	t.mu.Unlock()

	if status != StatusFailed && status != StatusCanceled {
		return TaskDTO{}, fmt.Errorf("任务状态为 %s，不允许重试", status)
	}
	return m.Enqueue(kind, chatID, chatTitle)
}

// createTaskRow 按任务当前快照在 store 中创建持久化记录
func (m *Manager) createTaskRow(t *task) error {
	dto := t.ToDTO()
	row := &store.TaskRow{
		ID:        dto.ID,
		Kind:      dto.Kind,
		ChatID:    dto.ChatID,
		ChatTitle: dto.ChatTitle,
		Status:    dto.Status,
		CreatedAt: dto.CreatedAt,
		StartedAt: dto.StartedAt,
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
	if err := m.store.UpdateTaskProgress(ctx, dto.ID,
		dto.Stats.Total, dto.Stats.Downloaded, dto.Stats.Failed, dto.Stats.Skipped,
		dto.Stats.TotalSize, dto.Stats.DownloadedSize); err != nil {
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
// 不调用 onChange（按设计 onChange 只在任务级生命周期转换时触发）。
func (m *Manager) handleRecordEvent(_ context.Context, evt downloader.RecordEvent) {
	if evt.Media != nil {
		m.mu.Lock()
		t := m.tasks[evt.Media.TaskID]
		m.mu.Unlock()
		if t != nil {
			t.applyRecordEvent(evt)
		}
	}
	select {
	case m.recordCh <- evt:
	default:
		m.logger.Warn("下载记录持久化队列已满，丢弃一条事件: status=%s", evt.Status)
	}
}
