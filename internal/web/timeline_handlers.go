package web

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tg-down/internal/timeline"
)

const (
	timelineDefaultLimit = 50
	timelineMaxLimit     = 200
	// timelineFreshTTL 是时间线索引的缓存有效期；下载新文件后点「刷新」强制重建
	timelineFreshTTL = 30 * time.Second
)

// handleTimeline 返回频道摘要列表 + 时间线条目（分页）。
//
// 参数：
//
//	chat=0 全部频道，否则只看该频道
//	before_date / before_msg_id  游标（取更旧消息），来自上一页的 next_cursor
//	tag 按 #标签 过滤，支持多个（逗号分隔，如 tag=马赛克,censored）
//	tag_mode=and 交集（同时含全部标签），缺省 or（含任一即可）
//	limit 页大小（默认 50，上限 200）
//	refresh=1 强制重建索引（新下载的文件立即可见）
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	chatID := parseQueryInt64(q.Get("chat"))
	beforeDate := parseQueryInt64(q.Get("before_date"))
	beforeMsgID := parseQueryInt64(q.Get("before_msg_id"))
	tags := parseTags(q.Get("tag"))
	matchAll := q.Get("tag_mode") == "and"
	limit := parseQueryInt(q.Get("limit"), timelineDefaultLimit)
	if limit <= 0 || limit > timelineMaxLimit {
		limit = timelineDefaultLimit
	}

	if q.Get("refresh") == "1" {
		s.timelineMu.Lock()
		err := s.timelineIndex.Rebuild(s.downloadRoot)
		s.timelineBuiltAt = time.Now()
		s.timelineMu.Unlock()
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "时间线索引构建失败: "+err.Error())
			return
		}
	} else {
		s.timelineEnsureFresh()
	}

	items, cursor, err := s.timelineIndex.Query(chatID, beforeDate, beforeMsgID, limit, tags, matchAll)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chats := s.timelineIndex.ChatList()

	var next string
	if cursor != nil {
		next = strconv.FormatInt(cursor.Date, 10) + "." + strconv.FormatInt(cursor.MessageID, 10)
	}
	if items == nil {
		items = []timeline.Entry{}
	}
	if chats == nil {
		chats = []timeline.ChatSummary{}
	}
	s.writeJSON(w, map[string]any{
		"chats":       chats,
		"items":       items,
		"next_cursor": next,
		"limit":       limit,
		"built_at":    s.timelineBuiltAt.Unix(),
	})
}

// timelineEnsureFresh 在缓存过期时重建索引（全表扫描加锁，几十万文件在秒级）
func (s *Server) timelineEnsureFresh() {
	s.timelineMu.Lock()
	defer s.timelineMu.Unlock()
	if time.Since(s.timelineBuiltAt) <= timelineFreshTTL {
		return
	}
	if err := s.timelineIndex.Rebuild(s.downloadRoot); err != nil {
		s.logger.Warn("时间线索引构建失败: %v", err)
	}
	s.timelineBuiltAt = time.Now()
}

// handleTimelineFile 提供时间线条目对应的媒体文件。
// 路径必须位于下载根目录内（ResolvePath 严格校验，防穿越）。
func (s *Server) handleTimelineFile(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	path, err := timeline.ResolvePath(s.downloadRoot, rel)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "文件不存在")
		return
	}

	f, err := os.Open(path) // #nosec G304 -- 已过 ResolvePath 根目录校验
	if err != nil {
		s.writeError(w, http.StatusNotFound, "文件不存在（可能已被移动或删除）")
		return
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.writeError(w, http.StatusNotFound, "文件不可读")
		return
	}

	name := filepath.Base(path)
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	setMediaResponseHeaders(w, mimeType, name)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func parseQueryInt64(v string) int64 {
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// parseTags 把逗号分隔的标签参数拆成去重列表（保留先后顺序）
func parseTags(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func parseQueryInt(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}