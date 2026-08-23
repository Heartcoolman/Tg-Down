// Package store 提供基于 SQLite 的任务与下载历史持久化能力，不依赖 web/telegram/queue。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
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
	// databaseFilePermission 防止聊天标题、消息 ID、文件路径和缩略图被同机其他用户读取。
	databaseFilePermission = 0600
	// currentSchemaVersion 是本版本期望的 schema 版本；低于它的库在 Open 时跑一次迁移并打上标记。
	// v2：history 增加 thumb_path / minithumb（画廊缩略图）。
	// v3：tasks 增加定时任务恢复与仅补失败文件所需的运行态字段。
	currentSchemaVersion = 3
)

// schemaTables 定义全部建表语句。
//
// 索引单独放在 schemaIndexes 中，且必须在补列迁移之后才创建：旧库里 history 表已存在
// （CREATE TABLE IF NOT EXISTS 是空操作）却没有新版列，此时若先建 unique_id 索引，
// SQLite 会报 "no such column"，整个 Open 失败——v1.x 的库将完全无法升级打开。
const schemaTables = `
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
	  filters            TEXT,
	  message_id         INTEGER NOT NULL DEFAULT 0,
	  stop_at_message_id INTEGER NOT NULL DEFAULT 0,
	  schedule_id        TEXT,
	  retry_failed_only  INTEGER NOT NULL DEFAULT 0
);

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
  thumb_path  TEXT,
  minithumb   BLOB,
  UNIQUE(chat_id, message_id)
);

CREATE TABLE IF NOT EXISTS schedules (
  id           TEXT PRIMARY KEY,
  chat_id      INTEGER NOT NULL,
  chat_title   TEXT,
  interval_min INTEGER NOT NULL,
  filters      TEXT,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_run     INTEGER,
  created_at   INTEGER NOT NULL,
  last_max_id  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);
`

// schemaIndexes 在补列迁移之后创建，因此可以安全地引用新版列（如 unique_id）。
//
// history 的三个筛选维度（media_type/status/chat_id）都恒与 "ORDER BY created_at DESC" 同用，
// 因此建成复合索引：单列索引只能定位行、排序仍要落到临时 B 树，复合索引让筛选与排序一次走完。
// 单列的 idx_history_chat_id 被 UNIQUE(chat_id, message_id) 的前缀完全覆盖，纯属白付写入成本，
// 此处不再创建（旧库里的那一个由 dropObsoleteIndexes 删除）。
const schemaIndexes = `
CREATE INDEX IF NOT EXISTS idx_tasks_status     ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks(created_at DESC);

CREATE INDEX IF NOT EXISTS idx_history_created_at ON history(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_history_type_time  ON history(media_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_history_status_time ON history(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_history_chat_time  ON history(chat_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_history_unique_id  ON history(unique_id);
CREATE INDEX IF NOT EXISTS idx_history_task_id    ON history(task_id, status);
CREATE INDEX IF NOT EXISTS idx_history_stats      ON history(media_type, status, file_size);
`

// obsoleteIndexes 是被复合索引取代的旧索引：留着只会拖慢每次写入
var obsoleteIndexes = []string{
	"idx_history_media_type",
	"idx_history_chat_id",
	"idx_history_status",
}

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
	if err := prepareDatabaseFiles(path); err != nil {
		return nil, err
	}

	// synchronous=NORMAL 在 WAL 下是安全的：断电最多丢失最近若干个已提交事务，数据库不会损坏。
	// 默认的 FULL 让每次提交都 fsync——每个媒体两次记录写入，几千个文件就是几千次 fsync。
	dsn := sqliteDSN(path)

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	if path == inMemoryDSN {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(maxOpenConnections)
	}

	// 顺序不可调换：建表 → 补列 → 建索引。索引可能引用旧库中尚不存在的列，
	// 必须等补列迁移完成后再建，否则 v1.x 的库会在 Open 阶段直接失败。
	ctx := context.Background()
	if err := checkSchemaCompatibility(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaTables); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化 schema 失败: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaIndexes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("创建索引失败: %w", err)
	}
	if err := dropObsoleteIndexes(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := hardenDatabaseFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// checkSchemaCompatibility 在执行任何当前版本的 DDL 前拒绝未来版本数据库，避免拒绝打开时
// 仍向较新格式的数据库添加旧表或旧列。没有 schema_version 表的旧库按版本 0 继续迁移。
func checkSchemaCompatibility(ctx context.Context, db *sql.DB) error {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='schema_version')`,
	).Scan(&exists); err != nil {
		return fmt.Errorf("检查数据库 schema 版本失败: %w", err)
	}
	if !exists {
		return nil
	}
	v, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if v > currentSchemaVersion {
		return fmt.Errorf("数据库 schema 版本 %d 高于当前支持的版本 %d，拒绝降级打开", v, currentSchemaVersion)
	}
	return nil
}

// migrate 把库升级到 currentSchemaVersion，已是最新则直接返回。
//
// 版本闸门不是形式主义：补列之外的两条归一化 UPDATE 是全表扫描，此前每次启动都无条件重跑一遍。
func migrate(ctx context.Context, db *sql.DB) error {
	v, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if v > currentSchemaVersion {
		return fmt.Errorf("数据库 schema 版本 %d 高于当前支持的版本 %d，拒绝降级打开", v, currentSchemaVersion)
	}
	if v == currentSchemaVersion {
		return nil
	}

	for _, step := range []func(context.Context, *sql.DB) error{
		migrateTasksTable, migrateHistoryTable, migrateSchedulesTable,
	} {
		if err := step(ctx, db); err != nil {
			return err
		}
	}
	return setSchemaVersion(ctx, db, currentSchemaVersion)
}

// sqliteDSN 使用 URL 结构化编码数据库路径，避免路径中的 ?/#/% 被 SQLite 当成 URI 参数。
func sqliteDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")

	// 内存库走 opaque 形态 file::memory:?...——"file://:memory:" 会把 ":memory:"
	// 当作 authority 被 SQLite 拒绝。
	if path == inMemoryDSN {
		return "file:" + inMemoryDSN + "?" + q.Encode()
	}

	// url.URL{Scheme:"file", Path:"./tg-down.db"} 会序列化成 "file://./tg-down.db"，
	// SQLite 的 URI 解析器把 "./" 当作 authority 并报 invalid uri authority——而默认
	// 存储路径恰是相对路径。因此进 URI 前先转绝对路径；Windows 盘符路径补前导斜杠，
	// 保证序列化成规范的 file:///C:/... 三斜杠形态。
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.ToSlash(path)
	if filepath.VolumeName(filepath.FromSlash(path)) != "" {
		path = "/" + path
	}

	u := &url.URL{Scheme: "file", Path: path}
	u.RawQuery = q.Encode()
	return u.String()
}

// schemaVersion 读取库的 schema 版本；空表（v2.x 及更早的库没有这张表）视为 0
func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	return int(v.Int64), nil
}

// setSchemaVersion 记录已应用的 schema 版本
func setSchemaVersion(ctx context.Context, db *sql.DB, v int) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, v); err != nil {
		return fmt.Errorf("记录 schema 版本失败: %w", err)
	}
	return nil
}

// dropObsoleteIndexes 删除被复合索引取代的旧索引
func dropObsoleteIndexes(ctx context.Context, db *sql.DB) error {
	for _, name := range obsoleteIndexes {
		if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+name); err != nil {
			return fmt.Errorf("删除过时索引 %s 失败: %w", name, err)
		}
	}
	return nil
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
		`stop_at_message_id INTEGER NOT NULL DEFAULT 0`,
		`schedule_id TEXT`,
		`retry_failed_only INTEGER NOT NULL DEFAULT 0`,
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
	for _, col := range []string{
		`unique_id TEXT`,
		`album_id INTEGER NOT NULL DEFAULT 0`,
		`thumb_path TEXT`,
		`minithumb BLOB`,
	} {
		if err := addColumnIfMissing(ctx, db, "history", col); err != nil {
			return err
		}
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

// migrateSchedulesTable 为既有库补充增量扫描水位列
func migrateSchedulesTable(ctx context.Context, db *sql.DB) error {
	return addColumnIfMissing(ctx, db, "schedules", `last_max_id INTEGER NOT NULL DEFAULT 0`)
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

// prepareDatabaseFiles 在 SQLite 首次读取或写入前创建并收紧主库权限；已有 WAL/SHM 文件也一并处理。
// 这样即使后续 schema 初始化失败，聊天标题、消息 ID 和文件路径也不会短暂暴露为宽松权限。
func prepareDatabaseFiles(path string) error {
	if path == "" || path == inMemoryDSN {
		return nil
	}
	//nolint:gosec // path 来自本地配置 store.path，非外部请求输入
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, databaseFilePermission)
	if err != nil {
		return fmt.Errorf("准备数据库文件失败 (%s): %w", path, err)
	}
	if err := f.Chmod(databaseFilePermission); err != nil {
		_ = f.Close()
		return fmt.Errorf("收紧数据库文件权限失败 (%s): %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭数据库预创建文件失败 (%s): %w", path, err)
	}
	for _, candidate := range []string{path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, databaseFilePermission); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("收紧数据库文件权限失败 (%s): %w", candidate, err)
		}
	}
	return nil
}

// hardenDatabaseFiles 收紧主库及 SQLite WAL 伴随文件权限。伴随文件可能按需出现，
// 当前不存在时忽略；SQLite 后续创建它们时会沿用主数据库的访问权限。
func hardenDatabaseFiles(path string) error {
	if path == "" || path == inMemoryDSN {
		return nil
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, databaseFilePermission); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("收紧数据库文件权限失败 (%s): %w", candidate, err)
		}
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
