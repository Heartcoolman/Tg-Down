package downloader

// HistorySpec 描述一次历史下载任务的执行参数，由 queue 组装、telegram 客户端消费。
// 定义在本包（叶子包）以避免 telegram <-> queue 的 import 环。
type HistorySpec struct {
	ChatID int64
	TaskID string
	// FromMessageID 是续扫游标（最后已扫描页的最旧 message_id），0 表示从最新消息开始
	FromMessageID int64
	// RetryMessageIDs 是恢复任务时需优先补下的消息（进程重启清扫的中断行，
	// 比游标更新，仅靠游标续扫会永久漏掉）
	RetryMessageIDs []int64
}
