package downloader

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	mediapkg "tg-down/internal/media"
)

// DefaultPathTemplate 是默认的落盘路径模板，等价于 v2.x 的硬编码布局。
//
// 默认值必须与旧版布局逐字节一致：改了它，所有既有用户的文件都会被认作"不存在"而重下一遍
// （v2.0 引入相册目录时就踩过这个坑）。只有用户显式改模板，落盘路径才会变。
const DefaultPathTemplate = "chat_{chat_id}/{type}/{album}/{name}"

// dateLayout 是 {date} 占位符的格式
const dateLayout = "2006-01-02"

// maxFileNameLen 是单个路径段的字节上限。多数文件系统的上限是 255 字节，
// 这里留出空间给去重后缀（"(12)"）与 sidecar 后缀（".json"）。
const maxFileNameLen = 200

// placeholderPattern 匹配 {xxx} 形式的占位符
var placeholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// 模板占位符名。集中定义，避免校验表与展开逻辑各写一份字面量而悄悄分叉。
const (
	phChatID    = "chat_id"
	phChatTitle = "chat_title"
	phType      = "type"
	phAlbum     = "album"
	phDate      = "date"
	phMsgID     = "msg_id"
	phSender    = "sender"
	phName      = "name"
	phExt       = "ext"
)

// knownPlaceholders 是模板支持的全部占位符。
var knownPlaceholders = map[string]bool{
	phChatID: true, phChatTitle: true, phType: true, phAlbum: true,
	phDate: true, phMsgID: true, phSender: true, phName: true, phExt: true,
}

// uniquePlaceholders 中至少要出现一个，模板才能保证同一聊天内不同消息不会撞到同一路径。
//
// {name} 由 telegram 层合成时已带上消息 id（docName/photo_<chat>_<msg>），因此二者都够用。
// 少了这层保证，两条消息里的 "report.pdf" 会解析到同一个路径：先到的文件会被后到的覆盖，
// 或被"文件已存在"逻辑当成已下载而静默跳过——两种结果都是数据丢失。
var uniquePlaceholders = []string{phName, phMsgID}

// ValidatePathTemplate 校验模板合法性，返回问题描述（合法时为空串）
func ValidatePathTemplate(tpl string) string {
	if strings.TrimSpace(tpl) == "" {
		return "路径模板不能为空"
	}
	if path.IsAbs(tpl) || filepath.IsAbs(tpl) {
		return "路径模板必须是相对路径（不能以 / 开头）"
	}
	if strings.Contains(tpl, "..") {
		return "路径模板不能包含 .."
	}
	if strings.Contains(tpl, `\`) {
		return `路径模板请统一用 / 分隔目录（Windows 上会自动转换）`
	}

	for _, m := range placeholderPattern.FindAllStringSubmatch(tpl, -1) {
		if !knownPlaceholders[m[1]] {
			return "未知的占位符: " + m[0]
		}
	}
	// 正确占位符移除后仍有大括号，说明存在大小写、连字符或未闭合等拼写错误。
	// 不能把它静默当作字面量，否则用户只会在下载后才发现路径完全不符合配置。
	if rest := placeholderPattern.ReplaceAllString(tpl, ""); strings.ContainsAny(rest, "{}") {
		return "路径模板包含格式错误的占位符"
	}
	// message_id 只在单个聊天内唯一，chat_title/sender/date 也都可能重复。
	// 下载根目录由所有聊天共享，因此必须把不可碰撞的 chat_id 放进最终路径；
	// {chat_title} 也可充当聊天隔离层——标题缺失时展开逻辑回退为 chat_<id>
	// （见 pathContext.value），不会让不同聊天的文件混进同一个目录。
	if !strings.Contains(tpl, "{"+phChatID+"}") && !strings.Contains(tpl, "{"+phChatTitle+"}") {
		return "路径模板必须包含 {chat_id} 或 {chat_title}，否则不同聊天的文件可能互相覆盖"
	}
	for _, p := range uniquePlaceholders {
		if strings.Contains(tpl, "{"+p+"}") {
			return ""
		}
	}
	return "路径模板必须包含 {name} 或 {msg_id}，否则同一聊天里不同消息的同名文件会互相覆盖"
}

// pathContext 是模板展开所需的上下文
type pathContext struct {
	media          *MediaInfo
	classifyByType bool
}

// expandPathTemplate 按模板生成相对于下载根目录的路径（含文件名）。
//
// 展开后为空的路径段会被丢弃：这正是默认模板复刻旧行为的方式——关闭类型分类时 {type} 展开为空，
// 不属于相册时 {album} 展开为空，对应的目录层级随之消失。
func expandPathTemplate(tpl string, ctx pathContext) string {
	segments := strings.Split(tpl, "/")
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		expanded := placeholderPattern.ReplaceAllStringFunc(seg, func(ph string) string {
			return ctx.value(strings.Trim(ph, "{}"))
		})
		expanded = sanitizeSegment(expanded)
		if expanded != "" {
			out = append(out, expanded)
		}
	}
	if len(out) == 0 {
		// 理论上不可达（模板必含 {name}/{msg_id}），兜底避免返回空路径把文件写到下载根目录上
		return sanitizeSegment(fallbackFileName(ctx.media))
	}
	return filepath.Join(out...)
}

// value 返回单个占位符的展开结果
func (c pathContext) value(name string) string {
	m := c.media
	switch name {
	case phChatID:
		return fmt.Sprintf("%d", m.ChatID)
	case phChatTitle:
		// 标题缺失时退回聊天 id：绝不能展开成空串，否则该路径段被丢弃，
		// 不同聊天的文件会混进同一个目录。
		if t := sanitizeSegment(m.ChatTitle); t != "" {
			return t
		}
		return fmt.Sprintf("chat_%d", m.ChatID)
	case phType:
		if !c.classifyByType {
			return "" // 段被丢弃，等价于关闭分类存储
		}
		return mediapkg.ClassifyDir(m.MediaType)
	case phAlbum:
		if m.AlbumID == 0 {
			return "" // 段被丢弃，非相册文件不进相册子目录
		}
		return fmt.Sprintf("album_%d", m.AlbumID)
	case phDate:
		if m.Date.IsZero() {
			return ""
		}
		return m.Date.Format(dateLayout)
	case phMsgID:
		return fmt.Sprintf("%d", m.MessageID)
	case phSender:
		if m.SenderID == 0 {
			return "unknown"
		}
		return fmt.Sprintf("%d", m.SenderID)
	case phName:
		return mediaFileName(m)
	case phExt:
		return fileExtension(m)
	default:
		return ""
	}
}

// mediaFileName 返回媒体的文件名（缺失时合成），未做路径段清理
func mediaFileName(m *MediaInfo) string {
	if m.FileName != "" {
		return m.FileName
	}
	return fallbackFileName(m)
}

// fallbackFileName 在 TDLib 未给出文件名时合成一个（带上消息 id 保证唯一）
func fallbackFileName(m *MediaInfo) string {
	return fmt.Sprintf("file_%d_%d%s", m.MessageID, m.TDFileID, fileExtension(m))
}

// fileExtension 返回带点的扩展名：优先取原文件名的后缀，其次按 MIME 推断，都没有则为空
func fileExtension(m *MediaInfo) string {
	if ext := filepath.Ext(m.FileName); ext != "" {
		return ext
	}
	return mediapkg.ExtensionFor(m.MimeType)
}

// windowsReserved 是 Windows 保留的设备名：以这些名字（无论大小写、无论带什么扩展名）
// 命名的文件在 Windows 上根本创建不出来。Telegram 上的文件名是攻击者可控的。
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// unsafeSegmentChars 是路径段中必须替换掉的字符（含 Windows 非法字符与路径分隔符）
var unsafeSegmentChars = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]`)

// sanitizeSegment 把任意字符串清理成一个安全的路径段。
//
// 除了常见的非法字符，还处理三类 Windows 专有的坑（M5 依赖这一项）：
//   - 保留设备名（CON/PRN/NUL/COM1…）：即便带扩展名也无法创建，加下划线前缀规避；
//   - 结尾的空格与点：Windows 会静默截断，导致写入的路径与记录的路径不一致；
//   - 超长名字：多数文件系统单段上限 255 字节，超出直接失败。
func sanitizeSegment(s string) string {
	s = unsafeSegmentChars.ReplaceAllString(s, "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.TrimSpace(s)

	if s == "" || s == "." {
		return ""
	}

	// 保留名判定只看扩展名之前的部分："nul.txt" 在 Windows 上同样不可用
	stem := s
	if i := strings.Index(s, "."); i > 0 {
		stem = s[:i]
	}
	if windowsReserved[strings.ToLower(stem)] {
		s = "_" + s
	}

	s = truncateSegment(s, maxFileNameLen)

	// Windows 会悄悄吃掉结尾的点和空格，使实际落盘名与我们记录的名字不一致
	s = strings.TrimRight(s, " .")
	if s == "" {
		return ""
	}
	return s
}

// truncateSegment 把路径段截断到 limit 字节，保留扩展名并避免切碎 UTF-8 字符
func truncateSegment(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	ext := filepath.Ext(s)
	if len(ext) > limit/2 { // 扩展名离谱地长，当作没有扩展名处理
		ext = ""
	}
	stem := s[:limit-len(ext)]
	// 避免从多字节字符中间截断，产生非法 UTF-8 文件名
	for stem != "" && !isUTF8Boundary(s, len(stem)) {
		stem = stem[:len(stem)-1]
	}
	return stem + ext
}

// UTF-8 续接字节形如 10xxxxxx：与掩码相与等于该前缀，即说明落在字符中间
const (
	utf8ContinuationMask   = 0xC0
	utf8ContinuationPrefix = 0x80
)

// isUTF8Boundary 报告 i 是否落在 UTF-8 字符边界上
func isUTF8Boundary(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	return s[i]&utf8ContinuationMask != utf8ContinuationPrefix
}
