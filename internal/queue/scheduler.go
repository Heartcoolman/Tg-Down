package queue

import (
	"context"
	"encoding/json"
	"time"

	"tg-down/internal/downloader"
)

const (
	// scheduleTickInterval 是定时计划的巡检周期
	scheduleTickInterval = time.Minute
	// MinScheduleIntervalMin 是定时计划允许的最小间隔（分钟），供 API 校验复用
	MinScheduleIntervalMin = 10
)

// runScheduler 周期性巡检定时计划，到期的计划触发一次历史下载任务；
// 随 Manager.Run 的 ctx 退出
func (m *Manager) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.fireDueSchedules(ctx)
		}
	}
}

// fireDueSchedules 触发所有到期的计划。先记录触发时间再入队：
// 若因同聊天已有任务被去重拒绝，也不会在下个 tick 立即热重试
func (m *Manager) fireDueSchedules(ctx context.Context) {
	rows, err := m.store.ListSchedules(ctx)
	if err != nil {
		m.logger.Warn("查询定时计划失败: %v", err)
		return
	}
	now := time.Now()
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		if r.LastRun != nil && now.Sub(*r.LastRun) < time.Duration(r.IntervalMin)*time.Minute {
			continue
		}
		if err := m.store.TouchScheduleLastRun(ctx, r.ID, now); err != nil {
			m.logger.Warn("更新定时计划触发时间失败: %v", err)
		}
		var filters downloader.HistoryFilters
		if r.Filters != "" {
			_ = json.Unmarshal([]byte(r.Filters), &filters) // 解析失败退化为不过滤
		}
		spec := downloader.HistorySpec{ChatID: r.ChatID, Filters: filters}
		if _, err := m.Enqueue(KindHistory, spec, r.ChatTitle); err != nil {
			// 常见于同聊天已有排队/运行中的任务，跳过本次触发
			m.logger.Info("定时计划 %s（聊天 %d）本次触发跳过: %v", r.ID, r.ChatID, err)
			continue
		}
		m.logger.Info("定时计划 %s 已触发聊天 %d 的历史下载", r.ID, r.ChatID)
	}
}
