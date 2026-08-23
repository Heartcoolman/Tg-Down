package web

import (
	"context"
	"net/http"
	"strings"

	"tg-down/internal/config"
	"tg-down/internal/logger"
	"tg-down/internal/notify"
)

// settingsDTO 完整设置快照（GET /api/settings）。
// 相比 v3.0 的只读三项，这里聚合了 config.yaml 中可在界面安全调整的运行项；
// Proxy 经 MaskProxyURL 脱敏后返回。
type settingsDTO struct {
	DownloadPath       string              `json:"download_path"`
	ClassifyByType     bool                `json:"classify_by_type"`
	MediaConcurrency   downloadSettingsDTO `json:"media_concurrency"`
	SaveMetadata       bool                `json:"save_metadata"`
	Proxy              string              `json:"proxy"`
	TaskConcurrency    int                 `json:"task_concurrency"`
	AutoRetry          int                 `json:"auto_retry"`
	LogLevel           string              `json:"log_level"`
	NotifyTelegramSelf bool                `json:"notify_telegram_self"`
	NotifyWebhookURL   string              `json:"notify_webhook_url"`
}

// updateSettingsRequest 是 POST /api/settings 的部分更新请求：
// 指针为 nil 的字段保持不变，全部字段可任意组合提交
type updateSettingsRequest struct {
	ClassifyByType     *bool   `json:"classify_by_type"`
	MaxConcurrent      *int    `json:"max_concurrent"`
	SaveMetadata       *bool   `json:"save_metadata"`
	Proxy              *string `json:"proxy"`
	TaskConcurrency    *int    `json:"task_concurrency"`
	AutoRetry          *int    `json:"auto_retry"`
	LogLevel           *string `json:"log_level"`
	NotifyTelegramSelf *bool   `json:"notify_telegram_self"`
	NotifyWebhookURL   *string `json:"notify_webhook_url"`
}

// settingsUpdateResponse 返回更新后的完整快照与提示语（重启生效等）
type settingsUpdateResponse struct {
	Settings settingsDTO `json:"settings"`
	Notices  []string    `json:"notices,omitempty"`
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.settingsSnapshot())
}

// handleSettingsClassify 是 v3.0 的旧端点，仅切换分类归档；新代码请用 POST /api/settings
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
	s.logger.Info("按媒体类型分类存储已%s", onOff(body.ClassifyByType))
	s.writeJSON(w, s.settingsSnapshot())
}

func (s *Server) settingsSnapshot() settingsDTO {
	return settingsDTO{
		DownloadPath:       s.client.DownloadPath(),
		ClassifyByType:     s.client.ClassifyByType(),
		MediaConcurrency:   downloadSettingsDTO{MaxConcurrent: s.client.DownloadConcurrency(), Active: s.client.ActiveDownloadCount()},
		SaveMetadata:       s.client.SaveMetadata(),
		Proxy:              config.MaskProxyURL(s.cfg.Telegram.Proxy),
		TaskConcurrency:    s.cfg.Queue.MaxConcurrentTasks,
		AutoRetry:          s.cfg.Queue.AutoRetryCount(),
		LogLevel:           strings.ToLower(strings.TrimSpace(s.cfg.Log.Level)),
		NotifyTelegramSelf: s.cfg.Notify.TelegramSelf,
		NotifyWebhookURL:   s.cfg.Notify.WebhookURL,
	}
}

// applyHotSettings 应用能立即生效的设置（分类存储、下载并发、元数据 sidecar）。
// 返回提示语；第二个返回值为 false 表示已写出错误响应，调用方须直接返回。
func (s *Server) applyHotSettings(w http.ResponseWriter, body *updateSettingsRequest) ([]string, bool) {
	var notices []string
	if body.ClassifyByType != nil {
		if err := s.client.SetClassifyByType(*body.ClassifyByType); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return nil, false
		}
		s.logger.Info("按媒体类型分类存储已%s", onOff(*body.ClassifyByType))
	}
	if body.MaxConcurrent != nil {
		if !s.applyConcurrency(*body.MaxConcurrent, w) {
			return nil, false
		}
	}
	if body.SaveMetadata != nil {
		if err := s.client.SetSaveMetadata(*body.SaveMetadata); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return nil, false
		}
		s.logger.Info("元数据 sidecar 已%s", onOff(*body.SaveMetadata))
		notices = append(notices, "元数据设置对后续下载生效")
	}
	return notices, true
}

// applyConfigSettings 写回只影响后续运行的设置（代理、任务并发、自动重试、日志级别、完成通知）。
// 返回提示语与"是否有配置需要落盘"；第三个返回值为 false 表示已写出错误响应。
func (s *Server) applyConfigSettings(
	w http.ResponseWriter, body *updateSettingsRequest,
) (notices []string, persist, ok bool) {
	cfg := s.cfg

	if body.Proxy != nil {
		v := strings.TrimSpace(*body.Proxy)
		if !config.IsValidProxy(v) {
			s.writeError(w, http.StatusBadRequest,
				"代理地址无效：支持 socks5://、http://（CONNECT）、mtproto:// 或 direct/off/none")
			return nil, false, false
		}
		cfg.Telegram.Proxy = v
		persist = true
		if v == "" {
			notices = append(notices, "代理已清除，重启后按环境变量回退规则生效")
		} else {
			notices = append(notices, "代理将在重启后生效")
		}
	}
	if body.TaskConcurrency != nil {
		if *body.TaskConcurrency <= 0 {
			s.writeError(w, http.StatusBadRequest, "任务并发数必须大于 0")
			return nil, false, false
		}
		cfg.Queue.MaxConcurrentTasks = *body.TaskConcurrency
		persist = true
		notices = append(notices, "任务并发数将在重启后生效")
	}
	if body.AutoRetry != nil {
		if *body.AutoRetry < 0 {
			s.writeError(w, http.StatusBadRequest, "自动重试次数不能为负（0 为关闭）")
			return nil, false, false
		}
		n := *body.AutoRetry
		cfg.Queue.AutoRetry = &n
		persist = true
		notices = append(notices, "自动重试次数将在重启后生效")
	}
	if body.LogLevel != nil {
		level := strings.ToLower(strings.TrimSpace(*body.LogLevel))
		switch level {
		case logger.LevelDebug, logger.LevelInfo, logger.LevelWarn, logger.LevelError:
		default:
			s.writeError(w, http.StatusBadRequest, "日志级别必须是 debug/info/warn/error")
			return nil, false, false
		}
		s.logger.SetLevel(level)
		cfg.Log.Level = level
		persist = true
	}
	if body.NotifyTelegramSelf != nil || body.NotifyWebhookURL != nil {
		if body.NotifyTelegramSelf != nil {
			cfg.Notify.TelegramSelf = *body.NotifyTelegramSelf
		}
		if body.NotifyWebhookURL != nil {
			cfg.Notify.WebhookURL = strings.TrimSpace(*body.NotifyWebhookURL)
		}
		persist = true
		s.rebuildNotifier()
		notices = append(notices, "完成通知设置对后续完成的任务生效")
	}
	return notices, persist, true
}

// handleSettingsUpdate 应用设置页提交的变更。能热应用的立即生效（分类/并发/元数据/日志级别/
// 完成通知），只能影响后续运行的（代理、任务并发、自动重试）写回配置并附重启提示。
func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var body updateSettingsRequest
	if !s.decode(w, r, &body) {
		return
	}
	notices, ok := s.applyHotSettings(w, &body)
	if !ok {
		return
	}
	configNotices, persist, ok := s.applyConfigSettings(w, &body)
	if !ok {
		return
	}
	notices = append(notices, configNotices...)

	if persist {
		if err := s.cfg.SaveConfig("config.yaml"); err != nil {
			s.logger.Warn("保存配置失败（当前会话内已应用）: %v", err)
			notices = append(notices, "设置已在本次运行中生效，但写入 config.yaml 失败")
		}
	}
	resp := settingsUpdateResponse{Settings: s.settingsSnapshot()}
	if len(notices) > 0 {
		resp.Notices = notices
	}
	s.writeJSON(w, resp)
}

// applyConcurrency 复用既有端点的校验与应用逻辑；校验或应用失败时已写出响应并返回 false
func (s *Server) applyConcurrency(n int, w http.ResponseWriter) bool {
	if n <= 0 {
		s.writeError(w, http.StatusBadRequest, "并发数量必须大于 0")
		return false
	}
	if err := s.client.SetDownloadConcurrency(n); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return true
}

// rebuildNotifier 按最新通知配置重建终结通知器并重新挂到队列上。
// 与 New() 中的初始接线保持同一模式；SetOnTerminal(nil) 表示关闭通知。
func (s *Server) rebuildNotifier() {
	var selfSend func(ctx context.Context, text string) error
	if s.cfg.Notify.TelegramSelf {
		selfSend = s.client.SendSelfMessage
	}
	if n := notify.New(selfSend, s.cfg.Notify.WebhookURL, s.logger); n != nil {
		s.queue.SetOnTerminal(n.TaskFinished)
	} else {
		s.queue.SetOnTerminal(nil)
	}
}

// onOff 布尔值的中文表述
func onOff(v bool) string {
	return map[bool]string{true: "开启", false: "关闭"}[v]
}
