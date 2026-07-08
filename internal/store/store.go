// Package store 提供基于 SQLite 的任务与下载历史持久化能力，不依赖 web/telegram/queue。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // 注册 database/sql 驱动 "sqlite"
)

const (
	// driverName 是 modernc.org/sqlite 注册的 database/sql 驱动名
	driverName = "sqlite"
	// busyTimeoutMillis 是连接遇锁等待重试的超时时间（毫秒）
	busyTimeoutMillis = 5000
	// maxOpenConnections 限制并发连接数：WAL 模式下多个连接可并发读，
	// 写操作由 SQLite 自身的单写者锁 + busy_timeout 串行化重试，
	// 无需把整个连接池压到 1 个连接，否则会牺牲历史查询等只读并发能力。
	maxOpenConnections = 4
	// inMemoryDSN 是 SQLite 匿名内存库路径，每个新连接默认互不可见，
	// 因此该场景下必须强制单连接，否则连接池新开的连接会看到空库。
	inMemoryDSN = ":memory:"
	// directoryPermission 是创建数据库父目录时使用的权限。
	directoryPermission = 0750
)

// schema 定义全部建表/索引语句，单一 schema 版本，无迁移框架
const schema = `
CREATE TABLE IF NOT EXISTS tasks (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,
  chat_id         INTEGER NOT NULL,
  chat_title      TEXT,
  status          TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER,
  error           TEXT,
  total           INTEGER DEFAULT 0,
  downloaded      INTEGER DEFAULT 0,
  failed          INTEGER DEFAULT 0,
  skipped         INTEGER DEFAULT 0,
  total_size      INTEGER DEFAULT 0,
  downloaded_size INTEGER DEFAULT 0,
  expected_total  INTEGER NOT NULL DEFAULT 0,
  scan_cursor     INTEGER NOT NULL DEFAULT 0,
  attempts        INTEGER NOT NULL DEFAULT 0,
  filters         TEXT,
  message_id      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tasks_status     ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks(created_at DESC);

CREATE TABLE IF NOT EXISTS history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id     TEXT,
  chat_id     INTEGER NOT NULL,
  chat_title  TEXT,
  message_id  INTEGER NOT NULL,
  media_type  TEXT NOT NULL,
  file_name   TEXT NOT NULL,
  file_path   TEXT NOT NULL,
  file_size   INTEGER NOT NULL,
  mime_type   TEXT,
  status      TEXT NOT NULL,
  reason      TEXT,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER,
  unique_id   TEXT,
  album_id    INTEGER NOT NULL DEFAULT 0,
  UNIQUE(chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_history_media_type ON history(media_type);
CREATE INDEX IF NOT EXISTS idx_history_chat_id    ON history(chat_id);
CREATE INDEX IF NOT EXISTS idx_history_status     ON history(status);
CREATE INDEX IF NOT EXISTS idx_history_created_at ON history(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_history_unique_id  ON history(unique_id);

CREATE TABLE IF NOT EXISTS schedules (
  id           TEXT PRIMARY KEY,
  chat_id      INTEGER NOT NULL,
  chat_title   TEXT,
  interval_min INTEGER NOT NULL,
  filters      TEXT,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_run     INTEGER,
  created_at   INTEGER NOT NULL
);
`

// Store 是基于 SQLite 的持久化句柄
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）指定路径的 SQLite 数据库并应用 schema。
//
// 连接池策略：通过 DSN 的 _pragma 参数开启 WAL + busy_timeout(5000ms)，
// WAL 模式允许多个连接并发读、单个连接写，写写冲突时由 SQLite 按
// busy_timeout 自动等待重试而非立即返回 "database is locked"；
// 因此 MaxOpenConns 设为较小的并发值（maxOpenConnections）而非 1，
// 以保留历史查询等只读路径的并发能力。匿名内存库（":memory:"）
// 是例外：每个新连接看到的是独立的空库，必须强制单连接，否则连接池
// 复用机制会导致数据“凭空丢失”。
func Open(path string) (*Store, error) {
	if err := ensureDatabaseDir(path); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, busyTimeoutMillis)

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	if path == inMemoryDSN {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(maxOpenConnections)
	}

	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化 schema 失败: %w", err)
	}

	if err := migrateTasksTable(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateHistoryTable(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// addColumnIfMissing 以 ALTER TABLE 幂等补列：新建库中该列已随 schema 存在，忽略重复列错误
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, columnDef string) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s`, table, columnDef))
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("迁移 %s 表失败: %w", table, err)
	}
	return nil
}

// migrateTasksTable 为既有库补充 v1.x 之后新增的列并归一状态词汇
func migrateTasksTable(ctx context.Context, db *sql.DB) error {
	for _, col := range []string{
		`expected_total INTEGER NOT NULL DEFAULT 0`,
		`scan_cursor INTEGER NOT NULL DEFAULT 0`,
		`attempts INTEGER NOT NULL DEFAULT 0`,
		`filters TEXT`,
		`message_id INTEGER NOT NULL DEFAULT 0`,
	} {
		if err := addColumnIfMissing(ctx, db, "tasks", col); err != nil {
			return err
		}
	}
	// v2.0 统一状态词汇：历史遗留的 pending 归一为 queued（幂等）
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='queued' WHERE status='pending'`); err != nil {
		return fmt.Errorf("迁移 tasks 状态词汇失败: %w", err)
	}
	return nil
}

// migrateHistoryTable 为既有库补充 unique_id 列与索引，并将旧版中断原因归一为常量
func migrateHistoryTable(ctx context.Context, db *sql.DB) error {
	if err := addColumnIfMissing(ctx, db, "history", `unique_id TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(ctx, db, "history", `album_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_history_unique_id ON history(unique_id)`); err != nil {
		return fmt.Errorf("创建 history unique_id 索引失败: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE history SET reason=? WHERE status='failed' AND reason='进程重启中断'`, HistoryReasonInterrupted); err != nil {
		return fmt.Errorf("迁移 history 中断原因失败: %w", err)
	}
	return nil
}

func ensureDatabaseDir(path string) error {
	if path == "" || path == inMemoryDSN {
		return nil
	}

	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}

	if err := os.MkdirAll(dir, directoryPermission); err != nil {
		return fmt.Errorf("创建数据库目录失败: %w", err)
	}
	return nil
}

// Close 关闭数据库连接
func (s *Store) Close() error {
	return s.db.Close()
}

// execContext 是内部统一的写操作封装，便于未来扩展（如统一错误包装）
func (s *Store) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}
