package downloader

// HistorySpec 描述一次历史下载任务的执行参数，由 queue 组装、telegram 客户端消费。
// 定义在本包（叶子包）以避免 telegram <-> queue 的 import 环。
type HistorySpec struct {
	ChatID int64
	TaskID string
	// FromMessageID 是续扫游标（最后已扫描页的最旧 message_id），0 表示从最新消息开始
	FromMessageID int64
	// MessageID 非 0 时为单消息下载任务（t.me 消息链接）：只下载该消息的媒体，不扫描历史
	MessageID int64
	// Filters 是任务级媒体过滤条件（零值 = 不过滤）
	Filters HistoryFilters
	// RetryMessageIDs 是恢复任务时需优先补下的消息（进程重启清扫的中断行，
	// 比游标更新，仅靠游标续扫会永久漏掉）
	RetryMessageIDs []int64
}

// HistoryFilters 是任务级媒体过滤条件；JSON 序列化后持久化在 tasks.filters 列，
// 并作为 POST /api/tasks 的 filters 字段
type HistoryFilters struct {
	// MediaTypes 是要下载的媒体类型子集（photo/video/document/animation/audio/voice），空 = 全部
	MediaTypes []string `json:"media_types,omitempty"`
	// DateFrom/DateTo 是消息日期区间（unix 秒，闭区间），0 = 不限
	DateFrom int64 `json:"date_from,omitempty"`
	DateTo   int64 `json:"date_to,omitempty"`
	// MaxFileSize 是单文件大小上限（字节），0 = 不限
	MaxFileSize int64 `json:"max_file_size,omitempty"`
}

// IsZero 报告过滤器是否为零值（不过滤）
func (f HistoryFilters) IsZero() bool {
	return len(f.MediaTypes) == 0 && f.DateFrom == 0 && f.DateTo == 0 && f.MaxFileSize == 0
}

// Match 报告一个媒体项（类型/消息日期 unix 秒/文件字节数）是否通过过滤
func (f HistoryFilters) Match(mediaType string, date, size int64) bool {
	if len(f.MediaTypes) > 0 {
		ok := false
		for _, t := range f.MediaTypes {
			if t == mediaType {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.DateFrom != 0 && date < f.DateFrom {
		return false
	}
	if f.DateTo != 0 && date > f.DateTo {
		return false
	}
	if f.MaxFileSize > 0 && size > f.MaxFileSize {
		return false
	}
	return true
}

// ValidMediaTypes 是 MediaTypes 的合法取值集合
var ValidMediaTypes = map[string]bool{
	"photo": true, "video": true, "document": true,
	"animation": true, "audio": true, "voice": true,
}

// Validate 校验过滤器字段合法性，返回首个问题的描述（合法时为空串）
func (f HistoryFilters) Validate() string {
	for _, t := range f.MediaTypes {
		if !ValidMediaTypes[t] {
			return "无效的媒体类型: " + t
		}
	}
	if f.DateFrom != 0 && f.DateTo != 0 && f.DateFrom > f.DateTo {
		return "date_from 不能晚于 date_to"
	}
	if f.MaxFileSize < 0 {
		return "max_file_size 不能为负"
	}
	return ""
}
