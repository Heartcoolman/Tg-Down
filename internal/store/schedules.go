package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ScheduleRow 表示 schedules 表中的一条定时下载计划
type ScheduleRow struct {
	ID          string     `json:"id"`
	ChatID      int64      `json:"chat_id"`
	ChatTitle   string     `json:"chat_title,omitempty"`
	IntervalMin int        `json:"interval_min"`
	Filters     string     `json:"filters,omitempty"` // downloader.HistoryFilters JSON
	Enabled     bool       `json:"enabled"`
	LastRun     *time.Time `json:"last_run,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// CreateSchedule 插入一条定时计划
func (s *Store) CreateSchedule(ctx context.Context, r *ScheduleRow) error {
	const q = `
INSERT INTO schedules (id, chat_id, chat_title, interval_min, filters, enabled, last_run, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.execContext(ctx, q,
		r.ID, r.ChatID, nullString(r.ChatTitle), r.IntervalMin, nullString(r.Filters),
		r.Enabled, timePtrToUnix(r.LastRun), timeToUnix(r.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("创建定时计划失败: %w", err)
	}
	return nil
}

// ListSchedules 返回全部定时计划，按创建时间倒序
//
//nolint:dupl // 与 ListTasks 结构同形但行类型/扫描器不同，泛型化收益低于可读性损失
func (s *Store) ListSchedules(ctx context.Context) ([]*ScheduleRow, error) {
	const q = `
SELECT id, chat_id, chat_title, interval_min, filters, enabled, last_run, created_at
FROM schedules ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询定时计划失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []*ScheduleRow
	for rows.Next() {
		r, err := scanScheduleRow(rows)
		if err != nil {
			return nil, fmt.Errorf("解析定时计划失败: %w", err)
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历定时计划失败: %w", err)
	}
	return items, nil
}

// DeleteSchedule 删除定时计划
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := s.execContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除定时计划失败: %w", err)
	}
	return checkRowsAffected(res, "定时计划", id)
}

// SetScheduleEnabled 启用/停用定时计划
func (s *Store) SetScheduleEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.execContext(ctx, `UPDATE schedules SET enabled = ? WHERE id = ?`, enabled, id)
	if err != nil {
		return fmt.Errorf("更新定时计划失败: %w", err)
	}
	return checkRowsAffected(res, "定时计划", id)
}

// TouchScheduleLastRun 记录定时计划的最近触发时间
func (s *Store) TouchScheduleLastRun(ctx context.Context, id string, at time.Time) error {
	res, err := s.execContext(ctx, `UPDATE schedules SET last_run = ? WHERE id = ?`, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("更新定时计划触发时间失败: %w", err)
	}
	return checkRowsAffected(res, "定时计划", id)
}

func scanScheduleRow(row scanner) (*ScheduleRow, error) {
	var (
		r                  ScheduleRow
		chatTitle, filters sql.NullString
		lastRun            sql.NullInt64
		createdAt          int64
	)
	if err := row.Scan(&r.ID, &r.ChatID, &chatTitle, &r.IntervalMin, &filters, &r.Enabled, &lastRun, &createdAt); err != nil {
		return nil, err
	}
	r.ChatTitle = chatTitle.String
	r.Filters = filters.String
	r.LastRun = nullInt64ToTimePtr(lastRun)
	r.CreatedAt = unixToTime(createdAt)
	return &r, nil
}
