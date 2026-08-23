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
	// historyQueueBuffer 是 history 任务等待队列的缓冲区大小；队列满时 Enqueue 立即返回错误。
	historyQueueBuffer = 256
	// recordQueueBuffer 是下载记录异步持久化队列的缓冲区大小，满载时投递方阻塞（背压，见 handleRecordEvent）
	recordQueueBuffer = 256
	// recordDrainTimeout 是 Manager 关闭时清空 recordCh 剩余积压的最长等待时间
	recordDrainTimeout = 5 * time.Second
	// persistInterval 是运行中任务进度周期性落盘的间隔，避免崩溃/硬杀丢失中途进度统计
	persistInterval = 10 * time.Second
	// retryBaseBackoff/retryMaxBackoff 界定任务级自动重试的指数退避区间
	retryBaseBackoff = 30 * time.Second
	retryMaxBackoff  = 5 * time.Minute
	retryQueuePoll   = 10 * time.Millisecond
)

// Manager 任务队列管理器：history 任务经有界 worker 池调度，monitor 任务独立运行，互不阻塞。
type Manager struct {
	client             ChatDownloader
	store              *store.Store
	logger             *logger.Logger
	maxConcurrentTasks int
	autoRetry          int // 任务级自动重试上限（0 = 关闭）
	recorder           func(context.Context, *downloader.RecordEvent)
	retryBackoff       func(attempt int) time.Duration // 可注入以便测试

	historyCh     chan *task
	recordCh      chan *downloader.RecordEvent // 下载记录持久化的异步队列，见 handleRecordEvent/recordWriter
	recordMu      sync.RWMutex                 // 与 recordWriter 关停同步，防止退出后仍向 recordCh 投递
	recordStopped bool
	// recordDone 在 recordWriter 退出后关闭，供 Run 等待所有已接受的记录落盘。
	recordDone chan struct{}

	// resumeHistory/resumeMonitor 由 loadTasks 收集、Run 启动时消费一次：
	// 进程重启前排队中/运行中的任务在此恢复续跑，而非回收为 failed
	resumeHistory []*task
	resumeMonitor *task

	mu          sync.Mutex
	tasks       map[string]*task
	order       []*task // 插入顺序（最早在前），List() 据此反转为最新优先
	monitorTask *task
	onChange    func(*TaskDTO)
	onTerminal  func(*TaskDTO) // 任务终结通知（completed/partial/最终 failed，取消与自动重试不触发）
	runCtx      context.Context
	stopped     bool           // 受 mu 保护；Run 关停后永久拒绝新任务
	draining    bool           // 受 mu 保护；登出等临时排空期间拒绝新任务，可由 EndDrain 恢复
	retryWG     sync.WaitGroup // 等待自动重试退避 goroutine 随 Run 的 ctx 一起退出

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
		recorder: store.NewRecorder(st, func(err error) {
			log.Warn("持久化下载历史失败: %v", err)
		}),
		retryBackoff: defaultRetryBackoff,
		historyCh:    make(chan *task, historyQueueBuffer),
		recordCh:     make(chan *downloader.RecordEvent, recordQueueBuffer),
		recordDone:   make(chan struct{}),
		tasks:        make(map[string]*task),
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
		m.restoreTaskRow(ctx, rows[i])
	}
}

// restoreTaskRow 把一行任务记录归位到内存状态：无法解析且处于进行中的 history 直接终结为
// failed；重启前排队/运行的 history 重置为 queued 并记入待恢复列表；running 的 monitor 待重启，
// 数据异常出现多个时只保留最新一个。调用方须持有 m.mu。
func (m *Manager) restoreTaskRow(ctx context.Context, row *store.TaskRow) {
	t, restoreErr := taskFromRow(row)
	if restoreErr != nil && t.kind == KindHistory &&
		(t.status == StatusQueued || t.status == StatusRunning) {
		t.status = StatusFailed
		t.errMsg = restoreErr.Error()
		now := time.Now()
		t.finishedAt = &now
		t.markDone()
		if err := m.store.UpdateTaskStatus(ctx, t.id, string(t.status), t.errMsg); err != nil {
			m.logger.Warn("持久化损坏任务状态失败: %v", err)
		}
		m.logger.Warn("任务 %s 无法恢复: %v", t.id, restoreErr)
		m.tasks[t.id] = t
		m.order = append(m.order, t)
		return
	}
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

// SetOnChange 设置任务生命周期变化回调（created/running/completed/failed/canceled），不逐文件触发
func (m *Manager) SetOnChange(fn func(*TaskDTO)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// SetOnTerminal 设置任务终结回调：completed、partial 与自动重试耗尽后的最终 failed 触发，
// canceled 与重试中的中间失败不触发；按任务粒度调用
func (m *Manager) SetOnTerminal(fn func(*TaskDTO)) {
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
		dto := t.ToDTO()
		fn(&dto)
	}
}

// Run 启动 history worker 池与记录持久化 writer，阻塞直至 ctx 取消；取消后停止接受新任务执行，
// 所有运行中任务的 ctx 均派生自 ctx，会随之自动取消。启动时一次性消费 loadTasks 收集的待恢复任务。
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	if ctx.Err() != nil {
		m.stopped = true
	}
	resumeHistory := m.resumeHistory
	resumeMonitor := m.resumeMonitor
	m.resumeHistory = nil
	m.resumeMonitor = nil
	m.mu.Unlock()

	recordStop := make(chan struct{})
	go func() {
		defer close(m.recordDone)
		m.recordWriter(recordStop)
	}()

	var auxWG sync.WaitGroup
	auxWG.Add(2)
	go func() {
		defer auxWG.Done()
		m.persistLoop(ctx)
	}()
	go func() {
		defer auxWG.Done()
		m.runScheduler(ctx)
	}()

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
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	// 先等所有 history worker 退出（不再产生记录事件），再让 recordWriter 清空剩余积压，
	// 避免 drain 在生产者仍在发事件时因通道瞬时为空而提前退出，丢失关停时的终态记录。
	wg.Wait()
	m.retryWG.Wait()
	m.markQueuedTasksDone()
	m.waitForMonitorStop()
	auxWG.Wait()
	m.recordMu.Lock()
	m.recordStopped = true
	close(recordStop)
	<-m.recordDone
	m.recordMu.Unlock()
}

// acceptingLocked 报告是否仍接受新任务。调用方必须持有 m.mu。
// 同时检查 runCtx 可消除 ctx 已取消但 Run 尚未抢到 m.mu 更新状态的短暂窗口。
func (m *Manager) acceptingLocked() bool {
	if m.stopped || m.draining {
		return false
	}
	if m.runCtx != nil && m.runCtx.Err() != nil {
		m.stopped = true
		return false
	}
	return true
}

// markQueuedTasksDone 结束本进程中不再会执行的排队轮次，但保留 queued 持久态供下次启动恢复。
func (m *Manager) markQueuedTasksDone() {
	m.mu.Lock()
	tasks := make([]*task, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()
	for _, t := range tasks {
		t.markDoneIfQueued()
	}
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

// waitForMonitorStop 与 monitor 切换串行，确保 Run 返回前当前 monitor 已完成最终持久化。
func (m *Manager) waitForMonitorStop() {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	m.mu.Lock()
	t := m.monitorTask
	m.mu.Unlock()
	if t != nil {
		<-t.done
	}
}

// beginHistoryTask 把 queued 任务切到 running 并派生任务级 ctx。
// 返回 false 表示任务已不在 queued（多为期间被取消），调用方直接退出。
func (m *Manager) beginHistoryTask(ctx context.Context, t *task) (context.Context, context.CancelFunc, bool) {
	t.mu.Lock()
	if t.status != StatusQueued {
		status := t.status
		t.mu.Unlock()
		// queued Cancel 负责在最终持久化与通知后关闭 done；worker 不得提前代关。
		if status != StatusCanceled {
			t.markDone()
		}
		return nil, nil, false
	}
	taskCtx, cancel := context.WithCancel(ctx)
	t.status = StatusRunning
	now := time.Now()
	t.startedAt = &now
	t.cancel = cancel
	t.mu.Unlock()

	m.persist(t)
	m.notify(t)
	return taskCtx, cancel, true
}

func (m *Manager) setExpectedTotal(t *task, total int64) {
	t.mu.Lock()
	t.expectedTotal = total
	t.mu.Unlock()
	m.persist(t)
}

// countHistoryTotal 在下载开始前统计媒体总数并落库+推送，前端立即可见“共约 N 个”。
// 单消息任务无需统计，总数恒为 1。
func (m *Manager) countHistoryTotal(
	ctx context.Context, t *task, isSingleMessage bool, filters downloader.HistoryFilters,
) {
	t.mu.Lock()
	t.phase = phaseCounting
	t.mu.Unlock()
	m.notify(t)

	switch {
	case isSingleMessage:
		m.setExpectedTotal(t, 1)
	case !filtersHaveExactCount(filters):
		// 现有计数接口只能应用媒体类型；其余过滤条件下写入未过滤总数会让完成进度远低于 100%。
		m.setExpectedTotal(t, 0)
	default:
		total, cntErr := m.client.CountHistoryMedia(ctx, t.chatID, filters.MediaTypes)
		switch {
		case cntErr != nil:
			if ctx.Err() == nil {
				m.logger.Warn("统计任务 %s 媒体总数失败，回退为未知总数: %v", t.id, cntErr)
			}
		case total > 0: // 0 = 服务端无法计数（如选中了贴纸），保持未知总数
			m.logger.Info("聊天 %d 共约 %d 个媒体文件", t.chatID, total)
			m.setExpectedTotal(t, total)
		}
	}
}

// buildHistorySpec 组装本轮扫描参数并切入下载阶段。
//
// 恢复或重试的任务先补下失败的行：这些消息可能比游标更新，仅靠游标续扫会永久漏掉。
// 不再限定“被重启清扫的中断行”——因网络/磁盘错误真失败的文件同样需要补下。
func (m *Manager) buildHistorySpec(ctx context.Context, t *task) *downloader.HistorySpec {
	t.mu.Lock()
	t.phase = phaseDownloading
	spec := &downloader.HistorySpec{
		ChatID:          t.chatID,
		ChatTitle:       t.chatTitle,
		TaskID:          t.id,
		FromMessageID:   t.scanCursor,
		MessageID:       t.messageID,
		Filters:         cloneHistoryFilters(t.filters),
		RetryOnly:       t.retryFailedOnly,
		StopAtMessageID: t.stopAtMessageID,
		ScheduleID:      t.scheduleID,
	}
	resumed := t.resumed
	t.mu.Unlock()
	m.notify(t)

	if resumed {
		if ids, listErr := m.store.ListFailedByTask(ctx, t.id); listErr != nil {
			m.logger.Warn("查询任务 %s 失败行失败: %v", t.id, listErr)
		} else if len(ids) > 0 {
			m.logger.Info("任务 %s 恢复：补下 %d 个失败的媒体", t.id, len(ids))
			spec.RetryMessageIDs = ids
		}
	}
	return spec
}

// runHistoryTask 执行单个 history 任务的完整生命周期：queued -> running -> completed/failed/canceled
func (m *Manager) runHistoryTask(ctx context.Context, t *task) {
	taskCtx, cancel, ok := m.beginHistoryTask(ctx, t)
	if !ok {
		return
	}

	t.mu.Lock()
	isSingleMessage := t.messageID != 0
	filters := cloneHistoryFilters(t.filters)
	t.mu.Unlock()

	m.countHistoryTotal(taskCtx, t, isSingleMessage, filters)
	spec := m.buildHistorySpec(taskCtx, t)

	result, err := m.client.DownloadHistoryMedia(taskCtx, spec)
	failedMedia := int64(0)
	if result != nil {
		failedMedia = result.Failed
	}

	canceled := taskCtx.Err() != nil
	shuttingDown := ctx.Err() != nil
	// 只有扫描与全部文件下载都成功后才推进水位；任务级错误或部分失败都必须留给下次计划重试。
	if result != nil && err == nil && failedMedia == 0 && !canceled &&
		spec.ScheduleID != "" && result.MaxMessageID > 0 {
		if updateErr := m.store.UpdateScheduleLastMaxID(taskCtx, spec.ScheduleID, result.MaxMessageID); updateErr != nil {
			m.logger.Warn("更新定时计划 %s 的扫描水位失败: %v", spec.ScheduleID, updateErr)
		}
	}
	retryScheduled, attempt := m.settleHistoryTask(t, err, failedMedia, canceled, shuttingDown)
	cancel()

	m.persist(t)
	m.notify(t)
	if retryScheduled {
		m.scheduleRetry(t, attempt, err)
		return // 任务未终结，不 markDone
	}
	if !canceled {
		m.fireTerminal(t) // completed / partial / 最终 failed
	}
	t.markDone()
}

// filtersHaveExactCount 报告现有 CountHistoryMedia 接口能否准确表达该过滤器。
func filtersHaveExactCount(f downloader.HistoryFilters) bool {
	return f.DateFrom == 0 && f.DateTo == 0 && f.MaxFileSize == 0 && f.Query == "" && f.SenderID == 0
}

// settleHistoryTask 依据本次运行的结果为任务定终态，返回是否已安排自动重试及当前尝试次数。
//
// 终态判定有两个独立来源：err 是任务级失败（扫描出错、聊天不可访问），failedMedia 是本次
// 运行中失败的文件数。二者都为空才算真正完成——只看 err 会把"扫描成功但文件全挂"报告成
// "已完成"，这正是 v2.0 的缺陷。
func (m *Manager) settleHistoryTask(
	t *task, err error, failedMedia int64, canceled, shuttingDown bool,
) (retryScheduled bool, attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	wasRetryFailedOnly := t.retryFailedOnly
	t.cancel = nil
	t.phase = ""
	t.scannedMessages = 0
	t.foundMedia = 0
	t.resumed = false
	t.retryFailedOnly = false

	canRetry := m.autoRetry > 0 && t.attempts < m.autoRetry
	finish := func(status Status, msg string) {
		finishedAt := time.Now()
		t.finishedAt = &finishedAt
		t.status = status
		t.errMsg = msg
	}
	// armRetry 让同一任务 id 续命（保留游标/统计），退避后重新入队
	armRetry := func(msg string, failedOnly bool) {
		t.attempts++
		t.status = StatusQueued
		t.errMsg = msg
		t.resumed = true // 重跑前补下本轮失败的行
		t.retryFailedOnly = failedOnly
		if failedOnly {
			// 失败计数清零，让统计反映"尚未解决的失败"而非跨重试的历史累计
			t.stats.Failed = 0
		}
		retryScheduled = true
	}

	switch {
	case shuttingDown:
		// 进程退出不是用户取消。保持可恢复状态，重启后沿用游标与本轮执行形态。
		t.status = StatusQueued
		t.errMsg = ""
		t.startedAt = nil
		t.finishedAt = nil
		t.resumed = true
		t.retryFailedOnly = wasRetryFailedOnly
	case canceled:
		finish(StatusCanceled, t.errMsg)
	case err != nil && canRetry:
		armRetry(err.Error(), false)
	case err != nil:
		finish(StatusFailed, err.Error()) // 自动重试耗尽的最终失败
	case failedMedia > 0 && canRetry:
		// 历史已完整扫完，只是部分文件失败：重试只补失败文件，不重扫历史
		armRetry(fmt.Sprintf("%d 个文件下载失败", failedMedia), true)
	case failedMedia > 0:
		// 保留游标；失败的文件仍记录在 history 中，可经重试补下
		finish(StatusPartial, fmt.Sprintf("%d 个文件下载失败", failedMedia))
	default:
		finish(StatusCompleted, "")
		t.scanCursor = 0 // 完整扫完且无失败，清游标
	}
	return retryScheduled, t.attempts
}

// scheduleRetry 在指数退避后把任务重新投入 history 队列；触发时若任务已被取消或管理器已关停则放弃
func (m *Manager) scheduleRetry(t *task, attempt int, cause error) {
	backoff := m.retryBackoff(attempt)
	m.logger.Warn("任务 %s 失败（%v），%s 后自动重试（第 %d/%d 次）", t.id, cause, backoff, attempt, m.autoRetry)
	m.mu.Lock()
	runCtx := m.runCtx
	m.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		return
	}
	m.retryWG.Add(1)
	go func() {
		defer m.retryWG.Done()
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-runCtx.Done():
			return
		}
		t.mu.Lock()
		stillQueued := t.status == StatusQueued
		t.mu.Unlock()
		if !stillQueued { // 退避期间被用户取消
			return
		}
		for {
			m.mu.Lock()
			if !m.acceptingLocked() {
				m.mu.Unlock()
				return
			}
			select {
			case m.historyCh <- t:
				m.mu.Unlock()
				return
			default:
				m.mu.Unlock()
			}
			select {
			case <-runCtx.Done():
				return
			case <-time.After(retryQueuePoll):
			}
		}
	}()
}

// Enqueue 创建并提交一个新任务。history 任务进入有界 worker 池排队；
// monitor 任务立即以独立 goroutine 长期运行（不占用 history 配额），ChatID 为 0 表示停止监控。
// spec 携带 ChatID 以及 history 任务的过滤器/单消息参数（monitor 忽略后两者）。
func (m *Manager) Enqueue(kind Kind, spec *downloader.HistorySpec, chatTitle string) (TaskDTO, error) {
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
func (m *Manager) enqueueHistory(spec *downloader.HistorySpec, chatTitle string) (TaskDTO, error) {
	m.mu.Lock()
	if !m.acceptingLocked() {
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务队列已停止")
	}
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

	select {
	case m.historyCh <- t:
		m.mu.Unlock()
		m.notify(t)
		return t.ToDTO(), nil
	default:
		m.removeTaskLocked(t)
		m.mu.Unlock()
		if err := m.store.DeleteTask(context.Background(), t.id); err != nil {
			m.logger.Warn("撤销未入队任务失败: %v", err)
			return TaskDTO{}, fmt.Errorf("任务队列已满，且撤销任务记录失败: %w", err)
		}
		return TaskDTO{}, fmt.Errorf("任务队列已满")
	}
}

// removeTaskLocked 撤销刚添加且尚未对外可见的任务；调用方必须持有 m.mu。
func (m *Manager) removeTaskLocked(t *task) {
	delete(m.tasks, t.id)
	for i := len(m.order) - 1; i >= 0; i-- {
		if m.order[i] == t {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

// enqueueMonitor 取消当前 monitor 任务（若有）并等待其退出，再视 chatID 决定是否启动新任务；
// chatID == 0 表示仅停止监控：返回被取消任务的快照（无任务时返回零值 TaskDTO），不创建新任务。
func (m *Manager) enqueueMonitor(chatID int64, chatTitle string) (TaskDTO, error) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()

	m.mu.Lock()
	if !m.acceptingLocked() {
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务队列已停止或正在排空")
	}
	prev := m.monitorTask
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

	t := newTask(KindMonitor, &downloader.HistorySpec{ChatID: chatID}, chatTitle)
	t.mu.Lock()
	t.status = StatusRunning
	now := time.Now()
	t.startedAt = &now
	t.mu.Unlock()

	m.mu.Lock()
	if !m.acceptingLocked() {
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务队列已停止或正在排空")
	}
	runCtx := m.runCtx
	if err := m.createTaskRow(t); err != nil {
		m.mu.Unlock()
		return TaskDTO{}, err
	}

	if runCtx == nil {
		runCtx = context.Background()
	}
	taskCtx, cancel := context.WithCancel(runCtx)
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()

	m.tasks[t.id] = t
	m.order = append(m.order, t)
	m.monitorTask = t
	m.mu.Unlock()

	// 同步建立 client 端关联，确保调用方一旦观察到任务状态为 running，
	// SetMonitorTask 必然已经生效，不会有 goroutine 异步设置带来的可见性竞争。
	m.client.SetMonitorTask(t.id, t.chatID, t.chatTitle)
	m.notify(t)

	go m.runMonitorTask(runCtx, taskCtx, t)
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

	m.client.SetMonitorTask(t.id, t.chatID, t.chatTitle)
	m.logger.Info("已恢复监控任务 %s（聊天 %d）", t.id, t.chatID)
	m.notify(t)
	go m.runMonitorTask(runCtx, taskCtx, t)
}

// runMonitorTask 阻塞至 ctx 取消，结束时清理 client 端关联并转为 canceled
func (m *Manager) runMonitorTask(runCtx, taskCtx context.Context, t *task) {
	<-taskCtx.Done()
	m.client.SetMonitorTask("", 0, "")

	t.mu.Lock()
	t.cancel = nil
	if runCtx.Err() != nil {
		// 进程退出后应自动恢复监控；只有显式用户取消才进入 canceled。
		t.status = StatusRunning
		t.errMsg = ""
		t.finishedAt = nil
	} else {
		t.status = StatusCanceled
		now := time.Now()
		t.finishedAt = &now
	}
	t.mu.Unlock()

	m.persist(t)
	m.notify(t)
	t.markDone()

	m.mu.Lock()
	if m.monitorTask == t {
		m.monitorTask = nil
	}
	m.mu.Unlock()
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

// BeginDrain 原子地暂停新任务接收，并返回当前仍有本轮执行尚未结束的任务 ID。
// 调用方应取消并 Wait 这些任务；失败时可调用 EndDrain 恢复接收。
func (m *Manager) BeginDrain() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || (m.runCtx != nil && m.runCtx.Err() != nil) {
		m.stopped = true
		return nil, fmt.Errorf("任务队列已停止")
	}
	if m.draining {
		return nil, fmt.Errorf("任务队列正在排空")
	}
	m.draining = true

	ids := make([]string, 0)
	for id, t := range m.tasks {
		t.mu.Lock()
		pending := !t.doneClosed
		t.mu.Unlock()
		if pending {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// EndDrain 恢复 BeginDrain 暂停的任务接收；永久关停后的 Manager 不可恢复。
func (m *Manager) EndDrain() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || (m.runCtx != nil && m.runCtx.Err() != nil) {
		m.stopped = true
		return fmt.Errorf("任务队列已停止")
	}
	m.draining = false
	return nil
}

// Wait 阻塞到任务的执行与最终持久化完成。ctx 取消时立即返回；任务不存在时返回错误。
// 调用方可先批量 Cancel，再逐个 Wait，确保所有下载都停止后再销毁底层 Telegram 客户端。
func (m *Manager) Wait(ctx context.Context, id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待任务 %s 结束: %w", id, ctx.Err())
	}
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
		if t.doneClosed {
			t.mu.Unlock()
			return fmt.Errorf("任务队列已停止，任务保留为 queued 供下次恢复")
		}
		t.status = StatusCanceled
		now := time.Now()
		t.finishedAt = &now
		t.mu.Unlock()
		m.persist(t)
		m.notify(t)
		t.markDone() // 最终持久化与通知完成后再允许 Wait 返回
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

// Retry 重新提交一个失败/取消/部分失败的任务。
//
// 部分失败（StatusPartial）的任务原地复用同一个任务 ID 重新入队，只补下失败的文件：
// 历史已经完整扫过，重扫一遍除了浪费时间不会有任何新发现，而失败的文件是按 task_id
// 记录在 history 表里的，换新 ID 就再也找不到它们了。
//
// 失败/取消的任务仍以新 ID 重新入队（旧任务行原样保留在历史列表中）。
func (m *Manager) Retry(id string) (TaskDTO, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return TaskDTO{}, fmt.Errorf("任务不存在: %s", id)
	}

	t.mu.Lock()
	status, kind, chatTitle, restoreErr := t.status, t.kind, t.chatTitle, t.restoreErr
	spec := &downloader.HistorySpec{
		ChatID: t.chatID, ChatTitle: t.chatTitle, Filters: cloneHistoryFilters(t.filters), MessageID: t.messageID,
		StopAtMessageID: t.stopAtMessageID, ScheduleID: t.scheduleID,
	}
	t.mu.Unlock()
	if restoreErr != "" {
		return TaskDTO{}, fmt.Errorf("任务持久化参数损坏，无法重试: %s", restoreErr)
	}

	switch status {
	case StatusPartial:
		return m.retryFailedFiles(t)
	case StatusFailed, StatusCanceled:
		return m.Enqueue(kind, spec, chatTitle)
	case StatusQueued, StatusRunning, StatusCompleted:
		return TaskDTO{}, fmt.Errorf("任务状态为 %s，不允许重试", status)
	default:
		return TaskDTO{}, fmt.Errorf("任务状态为 %s，不允许重试", status)
	}
}

// retryFailedFiles 原地重跑一个部分失败的任务，只补下 history 中记录的失败文件
func (m *Manager) retryFailedFiles(t *task) (TaskDTO, error) {
	m.mu.Lock()
	if !m.acceptingLocked() {
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务队列已停止或正在排空")
	}
	runCtx := m.runCtx
	m.mu.Unlock()

	// partial 状态会在上一轮的最终通知前可见，而 done 只在该轮全部清理结束后关闭。
	// 先等待旧 done，避免重试换上新通道后被上一轮迟到的 markDone 误关闭。
	t.mu.Lock()
	if t.status != StatusPartial {
		status := t.status
		t.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务状态为 %s，不允许重试", status)
	}
	previousDone := t.done
	t.mu.Unlock()
	<-previousDone
	if runCtx != nil && runCtx.Err() != nil {
		return TaskDTO{}, fmt.Errorf("任务队列已停止")
	}

	m.mu.Lock()
	if !m.acceptingLocked() {
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务队列已停止或正在排空")
	}
	t.mu.Lock()
	if t.status != StatusPartial {
		status := t.status
		t.mu.Unlock()
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务状态为 %s，不允许重试", status)
	}
	oldErrMsg := t.errMsg
	oldFinishedAt := t.finishedAt
	oldResumed := t.resumed
	oldRetryFailedOnly := t.retryFailedOnly
	oldAttempts := t.attempts
	oldFailed := t.stats.Failed
	t.status = StatusQueued
	t.errMsg = ""
	t.finishedAt = nil
	t.resumed = true         // 触发 ListFailedByTask 拉取待补下的消息
	t.retryFailedOnly = true // 跳过历史扫描
	t.attempts = 0           // 手动重试重置自动重试预算
	t.stats.Failed = 0

	select {
	case m.historyCh <- t:
		t.done = make(chan struct{})
		t.doneClosed = false
		dto := t.toDTOLocked()
		t.mu.Unlock()
		m.mu.Unlock()
		m.persist(t)
		m.notify(t)
		return dto, nil
	default:
		t.status = StatusPartial
		t.errMsg = oldErrMsg
		t.finishedAt = oldFinishedAt
		t.resumed = oldResumed
		t.retryFailedOnly = oldRetryFailedOnly
		t.attempts = oldAttempts
		t.stats.Failed = oldFailed
		t.mu.Unlock()
		m.mu.Unlock()
		return TaskDTO{}, fmt.Errorf("任务队列已满")
	}
}

// createTaskRow 按任务当前快照在 store 中创建持久化记录
func (m *Manager) createTaskRow(t *task) error {
	dto := t.ToDTO()
	row := &store.TaskRow{
		ID:              dto.ID,
		Kind:            dto.Kind,
		ChatID:          dto.ChatID,
		ChatTitle:       dto.ChatTitle,
		Status:          dto.Status,
		CreatedAt:       dto.CreatedAt,
		StartedAt:       dto.StartedAt,
		ExpectedTotal:   dto.ExpectedTotal,
		ScanCursor:      dto.ScanCursor,
		Attempts:        dto.Attempts,
		Filters:         t.filtersJSON(),
		MessageID:       dto.MessageID,
		StopAtMessageID: dto.StopAtMessageID,
		ScheduleID:      dto.ScheduleID,
		RetryFailedOnly: dto.RetryFailedOnly,
	}
	if err := m.store.CreateTask(context.Background(), row); err != nil {
		return fmt.Errorf("创建任务记录失败: %w", err)
	}
	return nil
}

// persist 将任务当前状态与统计快照写入 store；写入失败仅记录日志，不影响内存中的任务状态
func (m *Manager) persist(t *task) {
	// 快照与两条 UPDATE 必须对同一任务原子完成。否则周期性 persistRunning 取到的
	// running 快照可能后于任务终结时写入的 queued/completed 落库，把任务在库里退回
	// running：重启后 loadTasks 会把它当成"中断的任务"重跑一遍。
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	dto := t.ToDTO()
	ctx := context.Background()
	if err := m.store.UpdateTaskStatus(ctx, dto.ID, dto.Status, dto.Error); err != nil {
		m.logger.Warn("持久化任务状态失败: %v", err)
	}
	if err := m.store.UpdateTaskProgress(ctx, dto.ID, &store.TaskProgress{
		Total: dto.Stats.Total, Downloaded: dto.Stats.Downloaded,
		Failed: dto.Stats.Failed, Skipped: dto.Stats.Skipped,
		TotalSize: dto.Stats.TotalSize, DownloadedSize: dto.Stats.DownloadedSize,
		ExpectedTotal: dto.ExpectedTotal, ScanCursor: dto.ScanCursor, Attempts: dto.Attempts,
		RetryFailedOnly: dto.RetryFailedOnly,
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
func (m *Manager) handleRecordEvent(_ context.Context, evt *downloader.RecordEvent) {
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
	// 阻塞投递（背压）：recordWriter 是唯一消费者，FIFO 保证同一文件的 queued 先于其终态落盘。
	// 此前队列满时改走"同步落盘"旁路，恰好破坏了这个保证——终态可能抢在仍排队的 queued 之前
	// 写入，UpdateHistoryResult 命中 0 行，于是已下载的文件在历史里查无此记录。
	// 短暂阻塞下载 goroutine 远好过静默丢记录；写入本身是本地 SQLite，不会长时间卡住。
	m.recordMu.RLock()
	if m.recordStopped {
		m.recordMu.RUnlock()
		return
	}
	m.recordCh <- evt
	m.recordMu.RUnlock()
}
