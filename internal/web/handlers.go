package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"tg-down/internal/queue"
	"tg-down/internal/store"
	"tg-down/internal/telegram"
)

// routes 注册所有 HTTP 路由
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/chats", s.handleChats)
	mux.HandleFunc("POST /api/chats/refresh", s.handleChatsRefresh)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/auth/credentials", s.handleAuthCredentials)
	mux.HandleFunc("POST /api/auth/code", s.handleAuthCode)
	mux.HandleFunc("POST /api/auth/password", s.handleAuthPassword)
	mux.HandleFunc("POST /api/auth/abort", s.handleAuthAbort)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/settings/classify", s.handleSettingsClassify)
	mux.HandleFunc("GET /api/tasks", s.handleTasksList)
	mux.HandleFunc("POST /api/tasks", s.handleTasksCreate)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.handleTaskCancel)
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.handleTaskRetry)
	mux.HandleFunc("GET /api/download/settings", s.handleDownloadSettings)
	mux.HandleFunc("POST /api/download/concurrency", s.handleDownloadConcurrency)
	mux.HandleFunc("POST /api/media/{id}/pause", s.handleMediaPause)
	mux.HandleFunc("POST /api/media/{id}/resume", s.handleMediaResume)
	mux.HandleFunc("POST /api/media/pause-all", s.handleMediaPauseAll)
	mux.HandleFunc("POST /api/media/resume-all", s.handleMediaResumeAll)
	mux.HandleFunc("GET /api/history", s.handleHistoryList)
	mux.HandleFunc("GET /api/history/stats", s.handleHistoryStats)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.snapshot())
}

func (s *Server) handleChats(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	chats := s.chats
	s.mu.RUnlock()
	if chats == nil {
		chats = []telegram.ChatInfo{}
	}
	s.writeJSON(w, chats)
}

func (s *Server) handleChatsRefresh(w http.ResponseWriter, _ *http.Request) {
	if !s.requireReady(w) {
		return
	}
	s.refreshChats(context.Background())
	s.mu.RLock()
	chats := s.chats
	s.mu.RUnlock()
	if chats == nil {
		chats = []telegram.ChatInfo{}
	}
	s.writeJSON(w, chats)
}

func (s *Server) handleAuthCredentials(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIID   int    `json:"api_id"`
		APIHash string `json:"api_hash"`
		Phone   string `json:"phone"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	body.APIHash = strings.TrimSpace(body.APIHash)
	body.Phone = strings.TrimSpace(body.Phone)
	if body.APIID == 0 || body.APIHash == "" || body.Phone == "" {
		s.writeError(w, http.StatusBadRequest, "api_id、api_hash、手机号均不能为空")
		return
	}

	s.client.SetCredentials(body.APIID, body.APIHash, body.Phone)
	if err := s.client.SaveConfig(); err != nil {
		s.logger.Warn("保存配置失败（不影响本次登录）: %v", err)
	} else {
		s.logger.Info("已保存 API 凭据到 config.yaml")
	}

	select {
	case s.credCh <- struct{}{}:
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "登录请求处理中，请稍候")
	}
}

func (s *Server) handleAuthCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		s.writeError(w, http.StatusBadRequest, "验证码不能为空")
		return
	}
	if state, _ := s.currentState(); state != StateWaitingCode {
		s.writeError(w, http.StatusConflict, "当前不在等待验证码状态")
		return
	}
	select {
	case s.codeCh <- strings.TrimSpace(body.Code):
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "验证码提交处理中，请勿重复提交")
	}
}

func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Password == "" {
		s.writeError(w, http.StatusBadRequest, "密码不能为空")
		return
	}
	if state, _ := s.currentState(); state != StateWaitingPassword {
		s.writeError(w, http.StatusConflict, "当前不在等待密码状态")
		return
	}
	select {
	case s.passCh <- body.Password:
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "密码提交处理中，请勿重复提交")
	}
}

// handleAuthAbort 中止当前登录（验证码/密码步骤的"返回上一步"），回到凭据输入页
func (s *Server) handleAuthAbort(w http.ResponseWriter, _ *http.Request) {
	if state, _ := s.currentState(); state != StateWaitingCode && state != StateWaitingPassword {
		s.writeError(w, http.StatusConflict, "当前不在等待验证码/密码状态")
		return
	}
	select {
	case s.abortCh <- struct{}{}:
		s.writeOK(w)
	default:
		s.writeError(w, http.StatusConflict, "中止请求处理中，请勿重复提交")
	}
}

// handleAuthLogout 注销当前 Telegram 会话：先取消所有活动任务，再吊销授权并销毁本地会话，
// 最后通知 runTelegram 重新进入认证循环（回到凭据输入页）
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.requireReady(w) {
		return
	}
	for _, t := range s.queue.List() {
		if t.Status == string(queue.StatusQueued) || t.Status == string(queue.StatusRunning) {
			if err := s.queue.Cancel(t.ID); err != nil {
				s.logger.Warn("登出前取消任务 %s 失败: %v", t.ID, err)
			}
		}
	}
	if err := s.client.Logout(r.Context()); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	select {
	case s.logoutCh <- struct{}{}:
	default:
	}
	s.writeOK(w)
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.settingsSnapshot())
}

func (s *Server) handleSettingsClassify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClassifyByType bool `json:"classify_by_type"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.client.SetClassifyByType(body.ClassifyByType); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("按媒体类型分类存储已%s", map[bool]string{true: "开启", false: "关闭"}[body.ClassifyByType])
	s.writeJSON(w, s.settingsSnapshot())
}

func (s *Server) settingsSnapshot() settingsDTO {
	return settingsDTO{
		DownloadPath:   s.client.DownloadPath(),
		ClassifyByType: s.client.ClassifyByType(),
		MediaConcurrency: downloadSettingsDTO{
			MaxConcurrent: s.client.DownloadConcurrency(),
			Active:        s.client.ActiveDownloadCount(),
		},
	}
}

type settingsDTO struct {
	DownloadPath     string              `json:"download_path"`
	ClassifyByType   bool                `json:"classify_by_type"`
	MediaConcurrency downloadSettingsDTO `json:"media_concurrency"`
}

func (s *Server) handleTasksList(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.queue.List())
}

func (s *Server) handleTasksCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string `json:"kind"`
		ChatID int64  `json:"chat_id"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	kind := queue.Kind(body.Kind)
	if kind != queue.KindHistory && kind != queue.KindMonitor {
		s.writeError(w, http.StatusBadRequest, "kind 必须为 history 或 monitor")
		return
	}
	if !s.requireReady(w) {
		return
	}
	dto, err := s.queue.Enqueue(kind, body.ChatID, s.chatTitle(body.ChatID))
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, dto)
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.Cancel(r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleTaskRetry(w http.ResponseWriter, r *http.Request) {
	dto, err := s.queue.Retry(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeJSON(w, dto)
}

func (s *Server) handleDownloadSettings(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, downloadSettingsDTO{
		MaxConcurrent: s.client.DownloadConcurrency(),
		Active:        s.client.ActiveDownloadCount(),
	})
}

func (s *Server) handleDownloadConcurrency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxConcurrent int `json:"max_concurrent"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.MaxConcurrent <= 0 {
		s.writeError(w, http.StatusBadRequest, "并发数量必须大于 0")
		return
	}
	if err := s.client.SetDownloadConcurrency(body.MaxConcurrent); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, downloadSettingsDTO{
		MaxConcurrent: s.client.DownloadConcurrency(),
		Active:        s.client.ActiveDownloadCount(),
	})
}

func (s *Server) handleMediaPause(w http.ResponseWriter, r *http.Request) {
	if err := s.client.PauseMedia(r.Context(), r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleMediaResume(w http.ResponseWriter, r *http.Request) {
	if err := s.client.ResumeMedia(r.PathValue("id")); err != nil {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeOK(w)
}

func (s *Server) handleMediaPauseAll(w http.ResponseWriter, r *http.Request) {
	s.client.PauseAllMedia(r.Context())
	s.logger.Info("已暂停全部媒体下载")
	s.writeOK(w)
}

func (s *Server) handleMediaResumeAll(w http.ResponseWriter, _ *http.Request) {
	s.client.ResumeAllMedia()
	s.logger.Info("已继续全部媒体下载")
	s.writeOK(w)
}

// chatTitle 在已加载的聊天列表中按 ID 查找标题，未找到返回空串
func (s *Server) chatTitle(chatID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.chats {
		if c.ID == chatID {
			return c.Title
		}
	}
	return ""
}

func (s *Server) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	filter, page, pageSize, ok := s.parseHistoryFilter(w, r)
	if !ok {
		return
	}
	filter.Page = page
	filter.PageSize = pageSize

	items, total, err := s.store.QueryHistory(r.Context(), &filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos := make([]historyRecordDTO, len(items))
	for i, rec := range items {
		dtos[i] = toHistoryRecordDTO(rec)
	}
	s.writeJSON(w, historyListResponse{Items: dtos, Total: total, Page: page, PageSize: pageSize})
}

func (s *Server) handleHistoryStats(w http.ResponseWriter, r *http.Request) {
	filter, _, _, ok := s.parseHistoryFilter(w, r)
	if !ok {
		return
	}
	stats, err := s.store.HistoryStats(r.Context(), &filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos := make([]mediaTypeStatDTO, len(stats))
	for i, st := range stats {
		dtos[i] = toMediaTypeStatDTO(st)
	}
	s.writeJSON(w, historyStatsResponse{ByType: dtos})
}

// parseHistoryFilter 解析 /api/history 与 /api/history/stats 共用的查询参数；
// from/to 接受 RFC3339 或 unix 秒两种格式
func (s *Server) parseHistoryFilter(w http.ResponseWriter, r *http.Request) (filter store.HistoryFilter, page, pageSize int, ok bool) {
	q := r.URL.Query()
	filter.MediaType = q.Get("type")
	filter.Status = q.Get("status")
	filter.Query = q.Get("q")

	if v := q.Get("chat_id"); v != "" {
		chatID, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "chat_id 格式错误")
			return filter, 0, 0, false
		}
		filter.ChatID = chatID
	}

	if t, err := parseHistoryTime(q.Get("from")); err != nil {
		s.writeError(w, http.StatusBadRequest, "from 格式错误，需为 RFC3339 或 unix 秒")
		return filter, 0, 0, false
	} else if t != nil {
		filter.From = t
	}
	if t, err := parseHistoryTime(q.Get("to")); err != nil {
		s.writeError(w, http.StatusBadRequest, "to 格式错误，需为 RFC3339 或 unix 秒")
		return filter, 0, 0, false
	} else if t != nil {
		filter.To = t
	}

	page = 1
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "page 格式错误")
			return filter, 0, 0, false
		}
		page = n
	}
	pageSize = 0
	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "page_size 格式错误")
			return filter, 0, 0, false
		}
		pageSize = n
	}
	return filter, page, pageSize, true
}

// parseHistoryTime 解析 RFC3339 或 unix 秒时间戳，空串返回 nil
func parseHistoryTime(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
		t := time.Unix(sec, 0)
		return &t, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	ch := s.hub.add()
	defer s.hub.remove(ch)

	if data, err := json.Marshal(s.snapshot()); err == nil {
		writeSSE(w, flusher, sseMessage{Event: eventState, Data: string(data)})
	}

	for {
		select {
		case msg, alive := <-ch:
			if !alive {
				return
			}
			writeSSE(w, flusher, msg)
		case <-r.Context().Done():
			return
		}
	}
}

// --- 下载历史 DTOs ---

type historyRecordDTO struct {
	ID         int64  `json:"id"`
	TaskID     string `json:"task_id,omitempty"`
	ChatID     int64  `json:"chat_id"`
	ChatTitle  string `json:"chat_title,omitempty"`
	MessageID  int64  `json:"message_id"`
	MediaType  string `json:"media_type"`
	FileName   string `json:"file_name"`
	FilePath   string `json:"file_path"`
	FileSize   int64  `json:"file_size"`
	MimeType   string `json:"mime_type,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	FinishedAt *int64 `json:"finished_at,omitempty"`
}

func toHistoryRecordDTO(rec *store.HistoryRecord) historyRecordDTO {
	dto := historyRecordDTO{
		ID:        rec.ID,
		TaskID:    rec.TaskID,
		ChatID:    rec.ChatID,
		ChatTitle: rec.ChatTitle,
		MessageID: rec.MessageID,
		MediaType: rec.MediaType,
		FileName:  rec.FileName,
		FilePath:  rec.FilePath,
		FileSize:  rec.FileSize,
		MimeType:  rec.MimeType,
		Status:    rec.Status,
		Reason:    rec.Reason,
		CreatedAt: rec.CreatedAt.Unix(),
	}
	if rec.FinishedAt != nil {
		sec := rec.FinishedAt.Unix()
		dto.FinishedAt = &sec
	}
	return dto
}

type historyListResponse struct {
	Items    []historyRecordDTO `json:"items"`
	Total    int                `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

type mediaTypeStatDTO struct {
	MediaType string `json:"media_type"`
	Count     int    `json:"count"`
	TotalSize int64  `json:"total_size"`
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
}

func toMediaTypeStatDTO(st store.MediaTypeStat) mediaTypeStatDTO {
	return mediaTypeStatDTO{
		MediaType: st.MediaType,
		Count:     st.Count,
		TotalSize: st.TotalSize,
		Completed: st.Completed,
		Failed:    st.Failed,
		Skipped:   st.Skipped,
	}
}

type historyStatsResponse struct {
	ByType []mediaTypeStatDTO `json:"by_type"`
}

// --- helpers ---

func (s *Server) requireReady(w http.ResponseWriter) bool {
	if state, _ := s.currentState(); state != StateReady {
		s.writeError(w, http.StatusConflict, "Telegram 尚未就绪")
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("写响应失败: %v", err)
	}
}

func (s *Server) writeOK(w http.ResponseWriter) {
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func writeSSE(w http.ResponseWriter, f http.Flusher, msg sseMessage) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, msg.Data)
	f.Flush()
}

// --- SSE hub ---

type sseMessage struct {
	Event string
	Data  string
}

type sseHub struct {
	mu   sync.Mutex
	subs map[chan sseMessage]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{subs: make(map[chan sseMessage]struct{})}
}

func (h *sseHub) add() chan sseMessage {
	ch := make(chan sseMessage, sseBufferSize)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) remove(ch chan sseMessage) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// broadcast 非阻塞地向所有订阅者推送（缓冲满则丢弃该条）
func (h *sseHub) broadcast(msg sseMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}
