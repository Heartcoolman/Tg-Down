package downloader

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tg-down/internal/logger"
)

func newTestDownloader(downloadPath string) *Downloader {
	return New(downloadPath, 1, logger.New(logger.LevelError))
}

// TestClassifyDir 校验媒体类型到分类子目录的映射
func TestClassifyDir(t *testing.T) {
	cases := map[string]string{
		"photo":     "photo",
		"document":  "document",
		"video":     "video",
		"animation": "animation",
		"audio":     "audio",
		"voice":     "voice",
		"sticker":   "other",
		"":          "other",
	}
	for mediaType, want := range cases {
		if got := classifyDir(mediaType); got != want {
			t.Errorf("classifyDir(%q) = %q, want %q", mediaType, got, want)
		}
	}
}

// TestDownloadMedia_ClassifyByType 校验开启/关闭分类时的目录结构
func TestDownloadMedia_ClassifyByType(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		dir := t.TempDir()
		d := newTestDownloader(dir)
		d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
			return os.WriteFile(filePath, []byte("data"), 0600)
		})

		media := &MediaInfo{MessageID: 1, ChatID: 100, MediaType: "photo", FileName: "a.jpg"}
		if err := d.DownloadMedia(context.Background(), media); err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}

		want := filepath.Join(dir, "chat_100", "a.jpg")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected file at %s, stat error: %v", want, err)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		dir := t.TempDir()
		d := newTestDownloader(dir)
		d.SetClassifyByType(true)
		d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
			return os.WriteFile(filePath, []byte("data"), 0600)
		})

		media := &MediaInfo{MessageID: 1, ChatID: 100, MediaType: "photo", FileName: "a.jpg"}
		if err := d.DownloadMedia(context.Background(), media); err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}

		want := filepath.Join(dir, "chat_100", "photo", "a.jpg")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected file at %s, stat error: %v", want, err)
		}
	})

	t.Run("enabled_unknown_type", func(t *testing.T) {
		dir := t.TempDir()
		d := newTestDownloader(dir)
		d.SetClassifyByType(true)
		d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
			return os.WriteFile(filePath, []byte("data"), 0600)
		})

		media := &MediaInfo{MessageID: 1, ChatID: 100, MediaType: "sticker", FileName: "a.webp"}
		if err := d.DownloadMedia(context.Background(), media); err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}

		want := filepath.Join(dir, "chat_100", "other", "a.webp")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected file at %s, stat error: %v", want, err)
		}
	})
}

// TestDownloadMedia_RecordFunc 校验下载历史记录回调在各分支的事件序列
func TestDownloadMedia_RecordFunc(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		d := newTestDownloader(dir)
		d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
			return os.WriteFile(filePath, []byte("data"), 0600)
		})

		var events []RecordEvent
		d.SetRecordFunc(func(_ context.Context, evt RecordEvent) {
			events = append(events, evt)
		})

		media := &MediaInfo{MessageID: 1, ChatID: 100, MediaType: "photo", FileName: "a.jpg"}
		if err := d.DownloadMedia(context.Background(), media); err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}

		wantStatuses := []RecordStatus{RecordStarted, RecordCompleted}
		assertStatuses(t, events, wantStatuses)
	})

	t.Run("failure", func(t *testing.T) {
		dir := t.TempDir()
		d := newTestDownloader(dir)
		wantErr := os.ErrPermission
		d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, _ string) error {
			return wantErr
		})

		var events []RecordEvent
		d.SetRecordFunc(func(_ context.Context, evt RecordEvent) {
			events = append(events, evt)
		})

		media := &MediaInfo{MessageID: 1, ChatID: 100, MediaType: "photo", FileName: "b.jpg"}
		if err := d.DownloadMedia(context.Background(), media); err == nil {
			t.Fatal("DownloadMedia() expected error, got nil")
		}

		wantStatuses := []RecordStatus{RecordStarted, RecordFailed}
		assertStatuses(t, events, wantStatuses)
		if events[1].Reason != wantErr.Error() {
			t.Errorf("RecordFailed.Reason = %q, want %q", events[1].Reason, wantErr.Error())
		}
	})

	t.Run("skip_existing", func(t *testing.T) {
		dir := t.TempDir()
		d := newTestDownloader(dir)
		d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
			return os.WriteFile(filePath, []byte("data"), 0600)
		})

		chatDir := filepath.Join(dir, "chat_100")
		if err := os.MkdirAll(chatDir, DirectoryPermission); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		existing := filepath.Join(chatDir, "c.jpg")
		if err := os.WriteFile(existing, []byte("existing"), 0600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		var events []RecordEvent
		d.SetRecordFunc(func(_ context.Context, evt RecordEvent) {
			events = append(events, evt)
		})

		media := &MediaInfo{MessageID: 1, ChatID: 100, MediaType: "photo", FileName: "c.jpg"}
		if err := d.DownloadMedia(context.Background(), media); err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}

		wantStatuses := []RecordStatus{RecordSkipped}
		assertStatuses(t, events, wantStatuses)
	})
}

func assertStatuses(t *testing.T, events []RecordEvent, want []RecordStatus) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d (events=%+v)", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].Status != w {
			t.Errorf("events[%d].Status = %q, want %q", i, events[i].Status, w)
		}
	}
}
