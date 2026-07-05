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
func NewRecorder(s *Store) func(context.Context, downloader.RecordEvent) {
	return func(ctx context.Context, evt downloader.RecordEvent) {
		if evt.Media == nil {
			return
		}

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
			})
		case downloader.RecordCompleted:
			_ = s.UpdateHistoryResult(ctx, evt.Media.ChatID, evt.Media.MessageID, HistoryStatusCompleted, "", evt.FilePath)
		case downloader.RecordFailed:
			_ = s.UpdateHistoryResult(ctx, evt.Media.ChatID, evt.Media.MessageID, HistoryStatusFailed, evt.Reason, evt.FilePath)
		}
	}
}
