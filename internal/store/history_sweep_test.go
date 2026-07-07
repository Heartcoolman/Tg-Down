package store

import (
	"context"
	"testing"
)

// TestSweepInterruptedHistory 验证启动清扫会把遗留的 downloading 行终结为 failed，
// 而已终态（completed/failed/skipped）的行保持不变。
func TestSweepInterruptedHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 两条 downloading（中断遗留），一条 completed（应保持）
	for _, m := range []int64{1, 2} {
		rec := &HistoryRecord{
			TaskID: "t", ChatID: 100, MessageID: m, MediaType: "photo",
			FileName: "a.jpg", FilePath: "/tmp/a.jpg", FileSize: 1, Status: HistoryStatusDownloading,
		}
		if err := s.UpsertHistoryStart(ctx, rec); err != nil {
			t.Fatalf("UpsertHistoryStart() error = %v", err)
		}
	}
	done := &HistoryRecord{
		TaskID: "t", ChatID: 100, MessageID: 3, MediaType: "photo",
		FileName: "b.jpg", FilePath: "/tmp/b.jpg", FileSize: 1, Status: HistoryStatusDownloading,
	}
	if err := s.UpsertHistoryStart(ctx, done); err != nil {
		t.Fatalf("UpsertHistoryStart() error = %v", err)
	}
	if err := s.UpdateHistoryResult(ctx, 100, 3, HistoryStatusCompleted, "", "/tmp/b.jpg"); err != nil {
		t.Fatalf("UpdateHistoryResult() error = %v", err)
	}

	n, err := s.SweepInterruptedHistory(ctx)
	if err != nil {
		t.Fatalf("SweepInterruptedHistory() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("swept rows = %d, want 2", n)
	}

	items, _, err := s.QueryHistory(ctx, &HistoryFilter{ChatID: 100})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	statusByMsg := map[int64]string{}
	for _, it := range items {
		statusByMsg[it.MessageID] = it.Status
	}
	if statusByMsg[1] != HistoryStatusFailed || statusByMsg[2] != HistoryStatusFailed {
		t.Fatalf("interrupted rows not failed: %+v", statusByMsg)
	}
	if statusByMsg[3] != HistoryStatusCompleted {
		t.Fatalf("completed row was altered: %s", statusByMsg[3])
	}

	// 再次清扫无中断行，应清理 0 条
	if n, err := s.SweepInterruptedHistory(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep = (%d, %v), want (0, nil)", n, err)
	}
}

// TestUpdateHistoryResult_CompletedNotDowngradedToFailed 验证终态守卫：
// 已 completed 的行不会被后续 failed 事件覆盖（并发下载同一消息的落败方），
// 但 failed -> completed（重试成功）仍允许。
func TestUpdateHistoryResult_CompletedNotDowngradedToFailed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rec := &HistoryRecord{
		TaskID: "t", ChatID: 100, MessageID: 1, MediaType: "photo",
		FileName: "a.jpg", FilePath: "/tmp/a.jpg", FileSize: 1, Status: HistoryStatusDownloading,
	}
	if err := s.UpsertHistoryStart(ctx, rec); err != nil {
		t.Fatalf("UpsertHistoryStart() error = %v", err)
	}
	if err := s.UpdateHistoryResult(ctx, 100, 1, HistoryStatusCompleted, "", "/tmp/a.jpg"); err != nil {
		t.Fatalf("UpdateHistoryResult(completed) error = %v", err)
	}
	// 落败方的 failed 事件晚到：不得把 completed 覆盖为 failed（UpdateHistoryResult
	// 因守卫命中 0 行，返回 not-found 类错误，调用方本就忽略）
	_ = s.UpdateHistoryResult(ctx, 100, 1, HistoryStatusFailed, "boom", "")

	items, _, err := s.QueryHistory(ctx, &HistoryFilter{ChatID: 100})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	if len(items) != 1 || items[0].Status != HistoryStatusCompleted {
		t.Fatalf("completed row was downgraded: %+v", items)
	}

	// failed -> completed（重试成功）仍应允许
	rec2 := &HistoryRecord{
		TaskID: "t", ChatID: 200, MessageID: 1, MediaType: "photo",
		FileName: "c.jpg", FilePath: "/tmp/c.jpg", FileSize: 1, Status: HistoryStatusDownloading,
	}
	if err := s.UpsertHistoryStart(ctx, rec2); err != nil {
		t.Fatalf("UpsertHistoryStart() error = %v", err)
	}
	if err := s.UpdateHistoryResult(ctx, 200, 1, HistoryStatusFailed, "net", ""); err != nil {
		t.Fatalf("UpdateHistoryResult(failed) error = %v", err)
	}
	if err := s.UpdateHistoryResult(ctx, 200, 1, HistoryStatusCompleted, "", "/tmp/c.jpg"); err != nil {
		t.Fatalf("UpdateHistoryResult(retry->completed) error = %v", err)
	}
	items, _, err = s.QueryHistory(ctx, &HistoryFilter{ChatID: 200})
	if err != nil {
		t.Fatalf("QueryHistory() error = %v", err)
	}
	if len(items) != 1 || items[0].Status != HistoryStatusCompleted {
		t.Fatalf("failed->completed retry not applied: %+v", items)
	}
}
