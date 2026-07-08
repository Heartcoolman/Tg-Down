package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// terminalTaskStatuses 是任务的终态集合，进入这些状态时记录 finished_at
var terminalTaskStatuses = map[string]bool{
	TaskStatusCompleted: true,
	TaskStatusFailed:    true,
	TaskStatusCanceled:  true,
}

// CreateTask 插入一条新任务记录
func (s *Store) CreateTask(ctx context.Context, t *TaskRow) error {
	const q = `
INSERT INTO tasks (id, kind, chat_id, chat_title, status, created_at, started_at, finished_at,
                    error, total, downloaded, failed, skipped, total_size, downloaded_size, expected_total)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.execContext(ctx, q,
		t.ID, t.Kind, t.ChatID, t.ChatTitle, t.Status, timeToUnix(t.CreatedAt),
		timePtrToUnix(t.StartedAt), timePtrToUnix(t.FinishedAt), nullString(t.Error),
		t.Total, t.Downloaded, t.Failed, t.Skipped, t.TotalSize, t.DownloadedSize, t.ExpectedTotal,
	)
	if err != nil {
		return fmt.Errorf("创建任务失败: %w", err)
	}
	return nil
}

// UpdateTaskStatus 更新任务状态与错误信息；状态首次转为 running 时记录 started_at，
// 转为终态（completed/failed/canceled）时记录 finished_at
func (s *Store) UpdateTaskStatus(ctx context.Context, id, status, errMsg string) error {
	const q = `
UPDATE tasks SET
  status = ?,
  error = ?,
  started_at = CASE WHEN ? = 1 AND started_at IS NULL THEN ? ELSE started_at END,
  finished_at = CASE WHEN ? = 1 THEN ? ELSE finished_at END
WHERE id = ?`

	now := time.Now().Unix()
	isRunning := status == TaskStatusRunning
	isTerminal := terminalTaskStatuses[status]

	res, err := s.execContext(ctx, q, status, nullString(errMsg), isRunning, now, isTerminal, now, id)
	if err != nil {
		return fmt.Errorf("更新任务状态失败: %w", err)
	}
	return checkRowsAffected(res, "任务", id)
}

// UpdateTaskProgress 更新任务的进度统计
func (s *Store) UpdateTaskProgress(
	ctx context.Context, id string, total, downloaded, failed, skipped int,
	totalSize, downloadedSize, expectedTotal int64,
) error {
	const q = `
UPDATE tasks SET total = ?, downloaded = ?, failed = ?, skipped = ?, total_size = ?, downloaded_size = ?,
  expected_total = ?
WHERE id = ?`

	res, err := s.execContext(ctx, q, total, downloaded, failed, skipped, totalSize, downloadedSize, expectedTotal, id)
	if err != nil {
		return fmt.Errorf("更新任务进度失败: %w", err)
	}
	return checkRowsAffected(res, "任务", id)
}

// ListTasks 返回全部任务，按创建时间倒序排列
func (s *Store) ListTasks(ctx context.Context) ([]*TaskRow, error) {
	const q = `
SELECT id, kind, chat_id, chat_title, status, created_at, started_at, finished_at,
       error, total, downloaded, failed, skipped, total_size, downloaded_size, expected_total
FROM tasks ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询任务列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*TaskRow
	for rows.Next() {
		t, err := scanTaskRow(rows)
		if err != nil {
			return nil, fmt.Errorf("解析任务记录失败: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历任务列表失败: %w", err)
	}
	return tasks, nil
}

// GetTask 按 ID 查询单个任务，不存在时返回 nil, nil（非错误）
func (s *Store) GetTask(ctx context.Context, id string) (*TaskRow, error) {
	const q = `
SELECT id, kind, chat_id, chat_title, status, created_at, started_at, finished_at,
       error, total, downloaded, failed, skipped, total_size, downloaded_size, expected_total
FROM tasks WHERE id = ?`

	row := s.db.QueryRowContext(ctx, q, id)
	t, err := scanTaskRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("查询任务失败: %w", err)
	}
	return t, nil
}

// scanTaskRow 从单行结果解析出 TaskRow
func scanTaskRow(row scanner) (*TaskRow, error) {
	var (
		t                     TaskRow
		createdAt             int64
		startedAt, finishedAt sql.NullInt64
		errMsg, chatTitle     sql.NullString
	)

	if err := row.Scan(
		&t.ID, &t.Kind, &t.ChatID, &chatTitle, &t.Status, &createdAt, &startedAt, &finishedAt,
		&errMsg, &t.Total, &t.Downloaded, &t.Failed, &t.Skipped, &t.TotalSize, &t.DownloadedSize,
		&t.ExpectedTotal,
	); err != nil {
		return nil, err
	}

	t.ChatTitle = chatTitle.String
	t.Error = errMsg.String
	t.CreatedAt = unixToTime(createdAt)
	t.StartedAt = nullInt64ToTimePtr(startedAt)
	t.FinishedAt = nullInt64ToTimePtr(finishedAt)
	return &t, nil
}
