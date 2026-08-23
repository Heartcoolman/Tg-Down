package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"tg-down/internal/downloader"
)

// newTestStore 创建一个基于临时文件的测试用 Store
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTaskCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	task := &TaskRow{
		ID:              "task-1",
		Kind:            "scan",
		ChatID:          100,
		ChatTitle:       "测试群组",
		Status:          TaskStatusQueued,
		CreatedAt:       created,
		ExpectedTotal:   42,
		StopAtMessageID: 321,
		ScheduleID:      "schedule-1",
		RetryFailedOnly: true,
	}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}

	got, err := s.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetTask() = nil, want task")
	}
	if got.Kind != "scan" || got.ChatTitle != "测试群组" || got.Status != TaskStatusQueued || got.ExpectedTotal != 42 {
		t.Fatalf("GetTask() = %+v, mismatch", got)
	}
	if got.StopAtMessageID != 321 || got.ScheduleID != "schedule-1" || !got.RetryFailedOnly {
		t.Fatalf("task runtime fields did not round trip: %+v", got)
	}
	if got.StartedAt != nil || got.FinishedAt != nil {
		t.Fatalf("GetTask() StartedAt/FinishedAt should be nil initially, got %+v/%+v", got.StartedAt, got.FinishedAt)
	}

	if err := s.UpdateTaskStatus(ctx, "task-1", TaskStatusRunning, ""); err != nil {
		t.Fatalf("UpdateTaskStatus(running) error = %v", err)
	}
	got, err = s.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	if got.Status != TaskStatusRunning || got.StartedAt == nil {
		t.Fatalf("after running: got = %+v, want started_at set", got)
	}
	if got.FinishedAt != nil {
		t.Fatalf("after running: FinishedAt should still be nil, got %v", got.FinishedAt)
	}
	firstStartedAt := *got.StartedAt

	// 再次转为 running 不应覆盖已有 started_at
	if err := s.UpdateTaskStatus(ctx, "task-1", TaskStatusRunning, ""); err != nil {
		t.Fatalf("UpdateTaskStatus(running again) error = %v", err)
	}
	got, _ = s.GetTask(ctx, "task-1")
	if !got.StartedAt.Equal(firstStartedAt) {
		t.Fatalf("started_at changed on re-run transition: got %v, want %v", got.StartedAt, firstStartedAt)
	}

	if err := s.UpdateTaskProgress(ctx, "task-1", &TaskProgress{
		Total: 10, Downloaded: 6, Failed: 2, Skipped: 2,
		TotalSize: 1000, DownloadedSize: 600, ExpectedTotal: 50, ScanCursor: 12345, Attempts: 1,
		RetryFailedOnly: true,
	}); err != nil {
		t.Fatalf("UpdateTaskProgress() error = %v", err)
	}
	got, _ = s.GetTask(ctx, "task-1")
	if got.Total != 10 || got.Downloaded != 6 || got.Failed != 2 || got.Skipped != 2 || got.TotalSize != 1000 || got.DownloadedSize != 600 {
		t.Fatalf("progress mismatch: %+v", got)
	}
	if got.ExpectedTotal != 50 || got.ScanCursor != 12345 || got.Attempts != 1 {
		t.Fatalf("ExpectedTotal/ScanCursor/Attempts = %d/%d/%d, want 50/12345/1", got.ExpectedTotal, got.ScanCursor, got.Attempts)
	}
	if !got.RetryFailedOnly {
		t.Fatal("RetryFailedOnly progress flag was not persisted")
	}

	if err := s.UpdateTaskStatus(ctx, "task-1", TaskStatusFailed, "网络错误"); err != nil {
		t.Fatalf("UpdateTaskStatus(failed) error = %v", err)
	}
	got, _ = s.GetTask(ctx, "task-1")
	if got.Status != TaskStatusFailed || got.Error != "网络错误" || got.FinishedAt == nil {
		t.Fatalf("after failed: got = %+v", got)
	}

	// 第二个任务，验证 ListTasks 按 created_at 倒序
	task2 := &TaskRow{
		ID: "task-2", Kind: "scan", ChatID: 200, Status: TaskStatusQueued,
		CreatedAt: created.Add(time.Minute),
	}
	if err := s.CreateTask(ctx, task2); err != nil {
		t.Fatalf("CreateTask(task2) error = %v", err)
	}
	list, err := s.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "task-2" || list[1].ID != "task-1" {
		t.Fatalf("ListTasks() = %+v, want [task-2, task-1]", list)
	}
}

func TestPartialTaskRecordsFinishedAtAndRetryClearsIt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateTask(ctx, &TaskRow{
		ID: "partial-1", Kind: "history", ChatID: 1, Status: TaskStatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus(ctx, "partial-1", TaskStatusPartial, "one failed"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, "partial-1")
	if err != nil || got == nil || got.FinishedAt == nil {
		t.Fatalf("partial task must have finished_at: row=%+v err=%v", got, err)
	}
	if err := s.UpdateTaskStatus(ctx, "partial-1", TaskStatusQueued, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, "partial-1")
	if got.FinishedAt != nil || got.StartedAt != nil {
		t.Fatalf("requeued partial task retained terminal timestamps: %+v", got)
	}
}

// TestOpen_MigratesLegacyTasksTable 验证旧库（无 expected_total 列）经 Open 自动补列且可正常读写
func TestOpen_MigratesLegacyTasksTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open(driverName, "file:"+dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	const legacySchema = `
CREATE TABLE tasks (
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
  downloaded_size INTEGER DEFAULT 0
);
INSERT INTO tasks (id, kind, chat_id, status, created_at) VALUES ('legacy-1', 'history', 1, 'completed', 1);`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema error = %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db error = %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() on legacy db error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	got, err := s.GetTask(ctx, "legacy-1")
	if err != nil {
		t.Fatalf("GetTask(legacy-1) error = %v", err)
	}
	if got == nil || got.ExpectedTotal != 0 {
		t.Fatalf("GetTask(legacy-1) = %+v, want ExpectedTotal 0", got)
	}

	task := &TaskRow{ID: "new-1", Kind: "history", ChatID: 2, Status: TaskStatusQueued, CreatedAt: time.Now(), ExpectedTotal: 7}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	got, err = s.GetTask(ctx, "new-1")
	if err != nil || got == nil || got.ExpectedTotal != 7 {
		t.Fatalf("GetTask(new-1) = %+v, err = %v, want ExpectedTotal 7", got, err)
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "data", "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		t.Fatalf("expected parent directory to exist, stat error: %v", err)
	}
}

func TestOpenRestrictsDatabaseFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "private.db")
	// 模拟旧版本在宽松 umask 下留下的数据库文件。
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("expected SQLite file %s: %v", filepath.Base(candidate), err)
		}
		if got := info.Mode().Perm(); got != databaseFilePermission {
			t.Errorf("%s permissions = %o, want %o", filepath.Base(candidate), got, databaseFilePermission)
		}
	}
}

func TestOpenRestrictsPermissionsBeforeSchemaFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "invalid.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open(invalid database) unexpectedly succeeded")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != databaseFilePermission {
		t.Fatalf("database permissions after schema failure = %o, want %o", got, databaseFilePermission)
	}
}

func TestOpenSupportsSpecialCharactersInDatabasePath(t *testing.T) {
	// URI 特殊字符在 DSN 里必须被正确转义。`?` 在 Windows 是非法文件名字符，
	// 用 `% # 空格` 保持同等转义覆盖面。
	special := " ?#%"
	if runtime.GOOS == "windows" {
		special = " %#"
	}
	dir := filepath.Join(t.TempDir(), "data "+special)
	path := filepath.Join(dir, "store "+special+".db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(special path) error = %v", err)
	}
	if err := s.CreateTask(context.Background(), &TaskRow{
		ID: "special", Kind: "history", ChatID: 1, Status: TaskStatusQueued, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at the literal path: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen(special path) error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	row, err := s.GetTask(context.Background(), "special")
	if err != nil || row == nil {
		t.Fatalf("data missing after special-path reopen: row=%+v err=%v", row, err)
	}
}

// TestOpenSupportsRelativeDatabasePath 是 sqliteDSN 相对路径回归测试：相对路径拼进
// file URI 时（file://./x.db）SQLite 会把 "./" 当 authority 并报 invalid uri authority，
// 而默认存储路径正是相对的 ./tg-down.db。
func TestOpenSupportsRelativeDatabasePath(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	s, err := Open("./nested/tg-down.db")
	if err != nil {
		t.Fatalf("Open(relative path) error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(tmp, "nested", "tg-down.db")
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("database was not created at the resolved path: %v", err)
	}
}

func TestSqliteDSNConvertsRelativePathToAbsoluteURI(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	got := sqliteDSN("./data/tg-down.db")
	// 期望值与 sqliteDSN 同构：Windows 盘符路径需补前导斜杠（file:///C:/...）
	abs := filepath.ToSlash(filepath.Join(tmp, "data", "tg-down.db"))
	if filepath.VolumeName(filepath.FromSlash(abs)) != "" {
		abs = "/" + abs
	}
	want := "file://" + abs +
		"?_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29&_pragma=synchronous%28NORMAL%29"
	if got != want {
		t.Fatalf("sqliteDSN(relative) = %q, want %q", got, want)
	}
	if dsn := sqliteDSN(inMemoryDSN); dsn != "file::memory:?_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29&_pragma=synchronous%28NORMAL%29" {
		t.Fatalf("sqliteDSN(:memory:) 应为 opaque 形态（file::memory:），got %q", dsn)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetTask(context.Background(), "missing")
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	if got != nil {
		t.Fatalf("GetTask() = %+v, want nil", got)
	}
}

func TestUpdateTaskStatusNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateTaskStatus(context.Background(), "missing", TaskStatusRunning, ""); err == nil {
		t.Fatal("UpdateTaskStatus() on missing task should error")
	}
}

func TestHistoryUpsertAndUpdateIdempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec := &HistoryRecord{
		TaskID: "task-1", ChatID: 100, ChatTitle: "群A", MessageID: 1,
		MediaType: "photo", FileName: "a.jpg", FilePath: "/tmp/a.jpg",
		FileSize: 1024, MimeType: "image/jpeg", Status: HistoryStatusDownloading,
	}
	if err := s.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatalf("UpsertHistoryStart() error = %v", err)
	}
	// 重复开始（重扫场景），应原地更新而非插入新行
	if err := s.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatalf("UpsertHistoryStart() repeat error = %v", err)
	}

	items, total, err := s.QueryHistory(ctx, &HistoryFilter{ChatID: 100})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly 1 history row after duplicate upsert, got total=%d len=%d", total, len(items))
	}
	if items[0].Status != HistoryStatusDownloading {
		t.Fatalf("status = %s, want %s", items[0].Status, HistoryStatusDownloading)
	}

	if err := s.UpdateHistoryResult(ctx, 100, 1, HistoryStatusCompleted, "", "/tmp/a.jpg"); err != nil {
		t.Fatalf("UpdateHistoryResult() error = %v", err)
	}
	// 重复更新结果应保持幂等，不报错也不产生新行
	if err := s.UpdateHistoryResult(ctx, 100, 1, HistoryStatusCompleted, "", "/tmp/a.jpg"); err != nil {
		t.Fatalf("UpdateHistoryResult() repeat error = %v", err)
	}

	items, total, err = s.QueryHistory(ctx, &HistoryFilter{ChatID: 100})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly 1 history row after duplicate update, got total=%d len=%d", total, len(items))
	}
	if items[0].Status != HistoryStatusCompleted || items[0].FinishedAt == nil {
		t.Fatalf("after completed: got = %+v", items[0])
	}
}

func TestQueryHistoryIncludesGalleryMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mini := []byte{0xff, 0xd8, 0xff}
	rec := &HistoryRecord{
		TaskID: "task-gallery", ChatID: 100, ChatTitle: "相册群", MessageID: 8,
		MediaType: "photo", FileName: "a.jpg", FilePath: "/tmp/a.jpg", FileSize: 10,
		Status: HistoryStatusQueued, UniqueID: "unique-8", AlbumID: 77, Minithumb: mini,
	}
	if err := s.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateHistoryResult(ctx, 100, 8, HistoryStatusCompleted, "", rec.FilePath); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHistoryThumb(ctx, 100, 8, "/tmp/thumb.jpg"); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.QueryHistory(ctx, &HistoryFilter{ChatID: 100})
	if err != nil || len(items) != 1 {
		t.Fatalf("QueryHistory() items=%+v err=%v", items, err)
	}
	got := items[0]
	if got.AlbumID != 77 || got.ThumbPath != "/tmp/thumb.jpg" || got.UniqueID != "unique-8" ||
		!bytes.Equal(got.Minithumb, mini) {
		t.Fatalf("gallery metadata missing from QueryHistory: %+v", got)
	}
}

func TestRecorderReportsStoreErrors(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var recorded error
	recorder := NewRecorder(s, func(err error) { recorded = err })
	recorder(context.Background(), &downloader.RecordEvent{
		Media:  &downloader.MediaInfo{ChatID: 1, MessageID: 1, MediaType: "photo", FileName: "a.jpg"},
		Status: downloader.RecordQueued,
	})
	if recorded == nil {
		t.Fatal("recorder silently ignored a closed-database write error")
	}
}

// TestUpsertHistoryStart_DoesNotRegressTerminalStatus 验证重复扫描场景下，
// 已完成的记录不会被后续的 start/skip 事件回退为 downloading/skipped
func TestUpsertHistoryStart_DoesNotRegressTerminalStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec := &HistoryRecord{
		TaskID: "task-1", ChatID: 100, ChatTitle: "群A", MessageID: 1,
		MediaType: "photo", FileName: "a.jpg", FilePath: "/tmp/a.jpg",
		FileSize: 1024, MimeType: "image/jpeg", Status: HistoryStatusDownloading,
	}
	if err := s.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatalf("UpsertHistoryStart() error = %v", err)
	}
	if err := s.UpdateHistoryResult(ctx, 100, 1, HistoryStatusCompleted, "", "/tmp/a.jpg"); err != nil {
		t.Fatalf("UpdateHistoryResult() error = %v", err)
	}

	// 模拟重叠扫描再次命中同一 (chat_id, message_id)，文件已存在故记录为 skipped
	skipRec := *rec
	skipRec.Status = HistoryStatusSkipped
	if err := s.UpsertHistoryStart(ctx, &skipRec); err != nil {
		t.Fatalf("UpsertHistoryStart() repeat-after-completed error = %v", err)
	}

	items, total, err := s.QueryHistory(ctx, &HistoryFilter{ChatID: 100})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly 1 history row, got total=%d len=%d", total, len(items))
	}
	if items[0].Status != HistoryStatusCompleted {
		t.Fatalf("status regressed: got %s, want %s", items[0].Status, HistoryStatusCompleted)
	}
	if items[0].FinishedAt == nil {
		t.Fatal("finished_at was erased by a later start/skip upsert")
	}
}

func TestUpdateHistoryResultNotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateHistoryResult(context.Background(), 999, 1, HistoryStatusFailed, "未知错误", "")
	if err == nil {
		t.Fatal("UpdateHistoryResult() on missing row should error")
	}
}

// seedHistory 写入一组用于过滤/统计测试的历史记录
func seedHistory(t *testing.T, s *Store, base time.Time) {
	t.Helper()
	ctx := context.Background()
	rows := []struct {
		chatID                      int64
		msgID                       int64
		mediaType, fileName, taskID string
		status                      string
		size                        int64
		offset                      time.Duration
	}{
		{1, 1, "photo", "p1.jpg", "task-1", HistoryStatusCompleted, 100, 0},
		{1, 2, "video", "v1.mp4", "task-1", HistoryStatusFailed, 200, time.Minute},
		{1, 3, "photo", "p2.jpg", "task-2", HistoryStatusSkipped, 150, 2 * time.Minute},
		{2, 4, "document", "d1.pdf", "task-2", HistoryStatusCompleted, 300, 3 * time.Minute},
		{2, 5, "photo", "p3.jpg", "task-2", HistoryStatusCompleted, 120, 4 * time.Minute},
	}
	for _, r := range rows {
		rec := &HistoryRecord{
			TaskID: r.taskID, ChatID: r.chatID, MessageID: r.msgID,
			MediaType: r.mediaType, FileName: r.fileName, FilePath: "/tmp/" + r.fileName,
			FileSize: r.size, Status: HistoryStatusDownloading, CreatedAt: base.Add(r.offset),
		}
		if err := s.UpsertHistoryStart(ctx, rec); err != nil {
			t.Fatalf("UpsertHistoryStart(%v) error = %v", r, err)
		}
		if r.status != HistoryStatusDownloading {
			reason := ""
			if r.status == HistoryStatusFailed {
				reason = "下载失败"
			}
			if r.status == HistoryStatusSkipped {
				// 跳过场景直接重新 upsert 为 skipped 状态
				rec.Status = HistoryStatusSkipped
				if err := s.UpsertHistoryStart(ctx, rec); err != nil {
					t.Fatalf("UpsertHistoryStart(skip) error = %v", err)
				}
				continue
			}
			if err := s.UpdateHistoryResult(ctx, r.chatID, r.msgID, r.status, reason, "/tmp/"+r.fileName); err != nil {
				t.Fatalf("UpdateHistoryResult(%v) error = %v", r, err)
			}
		}
	}
}

func TestQueryHistoryFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedHistory(t, s, base)

	t.Run("by media type", func(t *testing.T) {
		items, total, err := s.QueryHistory(ctx, &HistoryFilter{MediaType: "photo"})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("total=%d len=%d, want 3", total, len(items))
		}
	})

	t.Run("by chat id", func(t *testing.T) {
		items, total, err := s.QueryHistory(ctx, &HistoryFilter{ChatID: 2})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total != 2 || len(items) != 2 {
			t.Fatalf("total=%d len=%d, want 2", total, len(items))
		}
	})

	t.Run("by status", func(t *testing.T) {
		items, total, err := s.QueryHistory(ctx, &HistoryFilter{Status: HistoryStatusCompleted})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("total=%d len=%d, want 3", total, len(items))
		}
	})

	t.Run("by query substring", func(t *testing.T) {
		items, total, err := s.QueryHistory(ctx, &HistoryFilter{Query: "jpg"})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		// p1.jpg, p2.jpg, p3.jpg 均含 "jpg"
		if total != 3 || len(items) != 3 {
			t.Fatalf("total=%d len=%d, want 3", total, len(items))
		}
	})

	t.Run("by date range", func(t *testing.T) {
		from := base.Add(90 * time.Second)
		items, total, err := s.QueryHistory(ctx, &HistoryFilter{From: &from})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("total=%d len=%d, want 3", total, len(items))
		}
		to := base.Add(90 * time.Second)
		items, total, err = s.QueryHistory(ctx, &HistoryFilter{To: &to})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total != 2 || len(items) != 2 {
			t.Fatalf("total=%d len=%d, want 2", total, len(items))
		}
	})

	t.Run("pagination and total", func(t *testing.T) {
		items, total, err := s.QueryHistory(ctx, &HistoryFilter{Page: 1, PageSize: 2})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total != 5 || len(items) != 2 {
			t.Fatalf("page1: total=%d len=%d, want total=5 len=2", total, len(items))
		}
		// 默认按 created_at 倒序：第一页应是最新的两条 (offset 4min, 3min)
		if items[0].FileName != "p3.jpg" || items[1].FileName != "d1.pdf" {
			t.Fatalf("page1 order = [%s, %s], want [p3.jpg, d1.pdf]", items[0].FileName, items[1].FileName)
		}

		items2, total2, err := s.QueryHistory(ctx, &HistoryFilter{Page: 3, PageSize: 2})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if total2 != 5 || len(items2) != 1 {
			t.Fatalf("page3: total=%d len=%d, want total=5 len=1", total2, len(items2))
		}

		// PageSize <= 0 应回退默认值且不报错
		itemsDefault, totalDefault, err := s.QueryHistory(ctx, &HistoryFilter{PageSize: 0})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if totalDefault != 5 || len(itemsDefault) != 5 {
			t.Fatalf("default page size: total=%d len=%d, want total=5 len=5", totalDefault, len(itemsDefault))
		}

		// PageSize 超过上限应被截断
		itemsCapped, _, err := s.QueryHistory(ctx, &HistoryFilter{PageSize: 1000})
		if err != nil {
			t.Fatalf("QueryHistory() error = %v", err)
		}
		if len(itemsCapped) != 5 {
			t.Fatalf("capped page size returned %d items, want 5 (only 5 rows exist)", len(itemsCapped))
		}
	})
}

func TestQueryHistoryTreatsLikeMetacharactersLiterally(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	names := []string{
		"literal%percent.txt", "literalXpercent.txt",
		"under_score.txt", "underXscore.txt",
		"bang!mark.txt", "bangXmark.txt",
	}
	for i, name := range names {
		if err := s.UpsertHistoryStart(ctx, &HistoryRecord{
			ChatID: 1, MessageID: int64(i + 1), MediaType: "document", FileName: name,
			FilePath: "/tmp/" + name, Status: HistoryStatusQueued,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tt := range []struct {
		query string
		want  string
	}{
		{query: "%", want: "literal%percent.txt"},
		{query: "_", want: "under_score.txt"},
		{query: "!", want: "bang!mark.txt"},
	} {
		t.Run(tt.query, func(t *testing.T) {
			items, total, err := s.QueryHistory(ctx, &HistoryFilter{Query: tt.query})
			if err != nil {
				t.Fatal(err)
			}
			if total != 1 || len(items) != 1 || items[0].FileName != tt.want {
				t.Fatalf("QueryHistory(%q) = total %d, items %+v", tt.query, total, items)
			}
		})
	}
}

func TestHistoryStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedHistory(t, s, base)

	stats, err := s.HistoryStats(ctx, &HistoryFilter{})
	if err != nil {
		t.Fatalf("HistoryStats() error = %v", err)
	}

	byType := make(map[string]MediaTypeStat, len(stats))
	for _, st := range stats {
		byType[st.MediaType] = st
	}

	photo, ok := byType["photo"]
	if !ok {
		t.Fatal("missing photo stat")
	}
	if photo.Count != 3 || photo.TotalSize != 100+150+120 || photo.Completed != 2 || photo.Skipped != 1 || photo.Failed != 0 {
		t.Fatalf("photo stat = %+v", photo)
	}

	video, ok := byType["video"]
	if !ok {
		t.Fatal("missing video stat")
	}
	if video.Count != 1 || video.Failed != 1 || video.Completed != 0 {
		t.Fatalf("video stat = %+v", video)
	}

	document, ok := byType["document"]
	if !ok {
		t.Fatal("missing document stat")
	}
	if document.Count != 1 || document.Completed != 1 {
		t.Fatalf("document stat = %+v", document)
	}

	// 过滤后聚合：仅 chat_id=2
	statsChat2, err := s.HistoryStats(ctx, &HistoryFilter{ChatID: 2})
	if err != nil {
		t.Fatalf("HistoryStats(chat=2) error = %v", err)
	}
	var totalCount int
	for _, st := range statsChat2 {
		totalCount += st.Count
	}
	if totalCount != 2 {
		t.Fatalf("HistoryStats(chat=2) total count = %d, want 2", totalCount)
	}
}
