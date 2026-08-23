package desktop

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gen2brain/beeep"

	"tg-down/internal/logger"
)

// 初始与上限退避：断流后从 initialReconnect 起倍增，封顶 maxReconnect
const (
	initialReconnect = 2 * time.Second
	maxReconnect     = time.Minute

	sseInitBufBytes = 64 << 10 // SSE 行扫描的初始缓冲
	sseMaxLineBytes = 1 << 20  // 单行上限，超过视为异常流
)

// TaskEvent 是 SSE task 事件载荷中本监视器关心的字段
type TaskEvent struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"chat_title"`
	Done   int64  `json:"done_files"`
	Total  int64  `json:"total_files"`
}

// WatchTaskFailures 订阅 baseURL 的 /api/events SSE 流，任务到达 failed/partial 终态时弹系统通知。
// 断流按指数退避重连直到 ctx 取消；阻塞运行，调用方放入 goroutine。
//
// 桌面端只对本地引擎启用该监视：远程实例的完成/失败通知由那台机器自身的 notify 配置负责，
// 同一事件不向两处重复打扰。
func WatchTaskFailures(ctx context.Context, baseURL string, log *logger.Logger) {
	client := &http.Client{} // 不设超时：SSE 为长连接，取消经 ctx 传播
	base := strings.TrimRight(baseURL, "/")
	backoff := initialReconnect
	for ctx.Err() == nil {
		err := consumeEvents(ctx, client, base, log)
		if ctx.Err() != nil {
			return
		}
		log.Warn("本地事件流断开，%s 后重连: %v", backoff, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < maxReconnect {
			backoff *= 2
			if backoff > maxReconnect {
				backoff = maxReconnect
			}
		}
	}
}

func consumeEvents(ctx context.Context, client *http.Client, base string, log *logger.Logger) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, sseInitBufBytes), sseMaxLineBytes)
	var event, data string
	reset := func() { event, data = "", "" }
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			// 空行 = 帧结束
			if event == "task" && data != "" {
				handleTaskEvent(data, log)
			}
			reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		default:
			// 注释或其他字段（如 retry:），忽略
		}
	}
	return scanner.Err()
}

func handleTaskEvent(data string, log *logger.Logger) {
	var t TaskEvent
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		return // 忽略坏帧
	}
	title := t.Title
	if title == "" {
		title = t.ID
	}
	var msg string
	switch t.Status {
	case "failed":
		msg = fmt.Sprintf("「%s」下载失败", title)
	case "partial":
		msg = fmt.Sprintf("「%s」部分完成（%d/%d 个文件）", title, t.Done, t.Total)
	default:
		return
	}
	log.Info("%s", msg)
	if err := beeep.Notify(AppName, msg, ""); err != nil {
		// 通知通道不可用（无通知中心/无 dbus 等）只降级为日志，不影响主流程
		log.Debug("系统通知发送失败: %v", err)
	}
}
