package store

import (
	"context"

	"tg-down/internal/downloader"
)

// NewRecorder 返回一个与 downloader.RecordFunc 兼容的回调，将每个下载事件
// 持久化为一条历史记录；CLI 模式直接使用，internal/queue.Manager 会在外层
// 包装它以额外更新内存中的任务统计。
//
// 权衡：本包不依赖 logger（保持持久层无外部依赖），且下载流程不能因历史记录
// 写入失败而失败/阻塞，因此这里对存储写入错误一律静默忽略，不 panic、不重试。
//
// 终态记录一律使用 context.Background() 落盘：调用方传入的 ctx 在取消/关停时可能已失效，
// 若沿用会使被取消下载的 RecordFailed 写入被 database/sql 直接中止，导致 history 行永久停在
// "downloading"。终态持久化必须不受请求 ctx 取消影响。
func NewRecorder(s *Store) func(context.Context, downloader.RecordEvent) {
	return func(_ context.Context, evt downloader.RecordEvent) {
		if evt.Media == nil {
			return
		}
		ctx := context.Background()

		switch evt.Status {
		case downloader.RecordStarted, downloader.RecordSkipped:
			status := HistoryStatusDownloading
			if evt.Status == downloader.RecordSkipped {
				status = HistoryStatusSkipped
			}
			_ = s.UpsertHistoryStart(ctx, &HistoryRecord{
				TaskID:    evt.Media.TaskID,
				ChatID:    evt.Media.ChatID,
				MessageID: evt.Media.MessageID,
				MediaType: evt.Media.MediaType,
				FileName:  evt.Media.FileName,
				FilePath:  evt.FilePath,
				FileSize:  evt.Media.FileSize,
				MimeType:  evt.Media.MimeType,
				Status:    status,
				UniqueID:  evt.Media.UniqueID,
			})
		case downloader.RecordCompleted:
			_ = s.UpdateHistoryResult(ctx, evt.Media.ChatID, evt.Media.MessageID, HistoryStatusCompleted, "", evt.FilePath)
		case downloader.RecordFailed:
			_ = s.UpdateHistoryResult(ctx, evt.Media.ChatID, evt.Media.MessageID, HistoryStatusFailed, evt.Reason, evt.FilePath)
		}
	}
}
