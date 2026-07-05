package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	// defaultHistoryPageSize 是 QueryHistory 在 PageSize<=0 时使用的默认分页大小
	defaultHistoryPageSize = 20
	// maxHistoryPageSize 是 QueryHistory 允许的最大分页大小
	maxHistoryPageSize = 100
)

// UpsertHistoryStart 在下载开始/跳过时写入或刷新一条历史记录，
// 以 (chat_id, message_id) 作为幂等键，使重复扫描不会产生重复行；
// 若已有记录处于终态（completed/failed），冲突更新被跳过，避免重复扫描将其回退为 downloading/skipped
func (s *Store) UpsertHistoryStart(ctx context.Context, rec *HistoryRecord) error {
	const q = `
INSERT INTO history (task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
                      file_size, mime_type, status, reason, created_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL)
ON CONFLICT(chat_id, message_id) DO UPDATE SET
  task_id    = excluded.task_id,
  chat_title = excluded.chat_title,
  media_type = excluded.media_type,
  file_name  = excluded.file_name,
  file_path  = excluded.file_path,
  file_size  = excluded.file_size,
  mime_type  = excluded.mime_type,
  status     = excluded.status,
  reason     = NULL,
  created_at = excluded.created_at,
  finished_at = NULL
WHERE history.status NOT IN ('completed', 'failed')`

	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.execContext(ctx, q,
		nullString(rec.TaskID), rec.ChatID, nullString(rec.ChatTitle), rec.MessageID,
		rec.MediaType, rec.FileName, rec.FilePath, rec.FileSize, nullString(rec.MimeType),
		rec.Status, timeToUnix(createdAt),
	)
	if err != nil {
		return fmt.Errorf("写入下载历史失败: %w", err)
	}
	return nil
}

// UpdateHistoryResult 按 (chat_id, message_id) 更新下载结果；状态为终态
// （completed/failed）时记录 finished_at；filePath 为空时保留原有路径不覆盖
func (s *Store) UpdateHistoryResult(ctx context.Context, chatID, messageID int64, status, reason, filePath string) error {
	const q = `
UPDATE history SET
  status = ?,
  reason = ?,
  file_path = COALESCE(NULLIF(?, ''), file_path),
  finished_at = CASE WHEN ? = 1 THEN ? ELSE finished_at END
WHERE chat_id = ? AND message_id = ?`

	isTerminal := status == "completed" || status == "failed"
	res, err := s.execContext(ctx, q, status, nullString(reason), filePath, isTerminal, time.Now().Unix(), chatID, messageID)
	if err != nil {
		return fmt.Errorf("更新下载历史失败: %w", err)
	}
	return checkRowsAffected(res, "下载历史", fmt.Sprintf("chat_id=%d,message_id=%d", chatID, messageID))
}

// historyFilterClause 根据过滤条件构建 WHERE 子句（不含 "WHERE" 关键字）与对应参数，
// 供 QueryHistory 与 HistoryStats 共用
func historyFilterClause(f *HistoryFilter) (where string, args []any) {
	var conds []string

	if f.MediaType != "" {
		conds = append(conds, "media_type = ?")
		args = append(args, f.MediaType)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		conds = append(conds, "file_name LIKE ?")
		args = append(args, "%"+f.Query+"%")
	}
	if f.ChatID != 0 {
		conds = append(conds, "chat_id = ?")
		args = append(args, f.ChatID)
	}
	if f.From != nil {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.From.Unix())
	}
	if f.To != nil {
		conds = append(conds, "created_at <= ?")
		args = append(args, f.To.Unix())
	}

	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// QueryHistory 按过滤条件分页查询下载历史，total 为忽略分页的匹配总数
func (s *Store) QueryHistory(ctx context.Context, f *HistoryFilter) ([]*HistoryRecord, int, error) {
	where, args := historyFilterClause(f)

	var total int
	var countQ strings.Builder
	countQ.WriteString("SELECT COUNT(*) FROM history ")
	countQ.WriteString(where)
	if err := s.db.QueryRowContext(ctx, countQ.String(), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计下载历史总数失败: %w", err)
	}

	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = defaultHistoryPageSize
	}
	if pageSize > maxHistoryPageSize {
		pageSize = maxHistoryPageSize
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	offset := (page - 1) * pageSize

	var q strings.Builder
	q.WriteString(`SELECT id, task_id, chat_id, chat_title, message_id, media_type, file_name, file_path,
       file_size, mime_type, status, reason, created_at, finished_at
FROM history `)
	q.WriteString(where)
	q.WriteString(` ORDER BY created_at DESC LIMIT ? OFFSET ?`)
	queryArgs := append(append([]any{}, args...), pageSize, offset)

	rows, err := s.db.QueryContext(ctx, q.String(), queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询下载历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []*HistoryRecord
	for rows.Next() {
		rec, err := scanHistoryRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("解析下载历史失败: %w", err)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历下载历史失败: %w", err)
	}
	return items, total, nil
}

// HistoryStats 按 media_type 分组统计下载历史，过滤条件与 QueryHistory 一致（忽略分页）
func (s *Store) HistoryStats(ctx context.Context, f *HistoryFilter) ([]MediaTypeStat, error) {
	where, args := historyFilterClause(f)

	var q strings.Builder
	q.WriteString(`SELECT media_type,
       COUNT(*),
       COALESCE(SUM(file_size), 0),
       SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END)
FROM history `)
	q.WriteString(where)
	q.WriteString(` GROUP BY media_type ORDER BY media_type`)

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("统计下载历史失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stats []MediaTypeStat
	for rows.Next() {
		var st MediaTypeStat
		if err := rows.Scan(&st.MediaType, &st.Count, &st.TotalSize, &st.Completed, &st.Failed, &st.Skipped); err != nil {
			return nil, fmt.Errorf("解析下载历史统计失败: %w", err)
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历下载历史统计失败: %w", err)
	}
	return stats, nil
}

// scanHistoryRow 从单行结果解析出 HistoryRecord
func scanHistoryRow(row scanner) (*HistoryRecord, error) {
	var (
		rec                     HistoryRecord
		taskID, chatTitle, mime sql.NullString
		reason                  sql.NullString
		createdAt               int64
		finishedAt              sql.NullInt64
	)

	if err := row.Scan(
		&rec.ID, &taskID, &rec.ChatID, &chatTitle, &rec.MessageID, &rec.MediaType, &rec.FileName,
		&rec.FilePath, &rec.FileSize, &mime, &rec.Status, &reason, &createdAt, &finishedAt,
	); err != nil {
		return nil, err
	}

	rec.TaskID = taskID.String
	rec.ChatTitle = chatTitle.String
	rec.MimeType = mime.String
	rec.Reason = reason.String
	rec.CreatedAt = unixToTime(createdAt)
	rec.FinishedAt = nullInt64ToTimePtr(finishedAt)
	return &rec, nil
}
