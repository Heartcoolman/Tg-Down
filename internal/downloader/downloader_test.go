package downloader

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tg-down/internal/logger"
)

func newTestDownloader(downloadPath string) *Downloader {
	return New(downloadPath, 1, logger.New(logger.LevelError))
}

// waitForStatus 轮询等待指定进度键到达目标状态，超时则失败。
func waitForStatus(t *testing.T, d *Downloader, id, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if d.progressStatus(id) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("progress %q did not reach status %q (got %q)", id, want, d.progressStatus(id))
}

// TestDownloader_PauseWhileQueued 覆盖“等待并发槽期间收到暂停”的场景：
// 修复前，媒体拿到槽后会被 markProgressStatus("downloading") 覆盖暂停并静默下载完成。
func TestDownloader_PauseWhileQueued(t *testing.T) {
	dir := t.TempDir()
	d := New(dir, 1, logger.New(logger.LevelError)) // 并发=1，仅一个下载槽

	block := make(chan struct{})
	firstStarted := make(chan struct{})
	secondRan := make(chan struct{}, 1)
	var firstOnce sync.Once

	d.SetDownloadFunc(func(_ context.Context, m *MediaInfo, filePath string) error {
		if m.MessageID == 1 {
			firstOnce.Do(func() { close(firstStarted) })
			<-block // 占住唯一下载槽，直到测试放行
			return os.WriteFile(filePath, []byte("a"), 0600)
		}
		select {
		case secondRan <- struct{}{}:
		default:
		}
		return os.WriteFile(filePath, []byte("b"), 0600)
	})

	first := &MediaInfo{TaskID: "t", MessageID: 1, TDFileID: 1, ChatID: 100, MediaType: "photo", FileName: "1.jpg"}
	second := &MediaInfo{TaskID: "t", MessageID: 2, TDFileID: 2, ChatID: 100, MediaType: "photo", FileName: "2.jpg"}

	done1 := make(chan error, 1)
	go func() { done1 <- d.DownloadMedia(context.Background(), first) }()
	<-firstStarted

	done2 := make(chan error, 1)
	go func() { done2 <- d.DownloadMedia(context.Background(), second) }()

	secondID := mediaProgressKey(second)
	waitForStatus(t, d, secondID, "queued") // 第二个已注册并在等待槽位

	if err := d.PauseMedia(context.Background(), secondID); err != nil {
		t.Fatalf("PauseMedia() error = %v", err)
	}
	close(block) // 放行第一个，腾出槽位
	if err := <-done1; err != nil {
		t.Fatalf("first download error = %v", err)
	}

	// 暂停中的第二个即使拿到槽也不得执行下载
	select {
	case <-secondRan:
		t.Fatal("排队期间被暂停的媒体仍执行了下载（暂停丢失）")
	case <-time.After(200 * time.Millisecond):
	}
	if st := d.progressStatus(secondID); st != "paused" {
		t.Fatalf("second status = %q, want paused", st)
	}

	// 恢复后应真正完成下载
	if err := d.ResumeMedia(secondID); err != nil {
		t.Fatalf("ResumeMedia() error = %v", err)
	}
	if err := <-done2; err != nil {
		t.Fatalf("second download error = %v", err)
	}
	select {
	case <-secondRan:
	case <-time.After(time.Second):
		t.Fatal("恢复后媒体未执行下载")
	}
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

func TestDownloader_MediaProgressSnapshot(t *testing.T) {
	dir := t.TempDir()
	d := New(dir, 1, logger.New(logger.LevelError))
	started := make(chan struct{})
	release := make(chan struct{})

	d.SetDownloadFunc(func(_ context.Context, media *MediaInfo, filePath string) error {
		d.UpdateProgress(media.TDFileID, 5, 10, false)
		close(started)
		<-release
		return os.WriteFile(filePath, []byte("data"), 0600)
	})

	media := &MediaInfo{
		TaskID:    "task-1",
		MessageID: 10,
		TDFileID:  20,
		ChatID:    30,
		MediaType: "photo",
		FileName:  "progress.jpg",
		FileSize:  10,
	}
	done := make(chan error, 1)
	go func() { done <- d.DownloadMedia(context.Background(), media) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}

	items := d.ActiveMedia()
	if len(items) != 1 {
		t.Fatalf("ActiveMedia() len = %d, want 1", len(items))
	}
	got := items[0]
	if got.Status != "downloading" {
		t.Errorf("Status = %q, want downloading", got.Status)
	}
	if got.DownloadedSize != 5 || got.FileSize != 10 {
		t.Errorf("progress = %d/%d, want 5/10", got.DownloadedSize, got.FileSize)
	}
	if got.Percent != 50 {
		t.Errorf("Percent = %v, want 50", got.Percent)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download did not finish")
	}
	if items := d.ActiveMedia(); len(items) != 0 {
		t.Fatalf("ActiveMedia() after finish len = %d, want 0", len(items))
	}
}

func TestDownloader_SetMaxConcurrent(t *testing.T) {
	dir := t.TempDir()
	d := New(dir, 1, logger.New(logger.LevelError))
	d.SetMaxConcurrent(2)

	var active int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
		if n := atomic.AddInt32(&active, 1); n > 2 {
			t.Errorf("active downloads = %d, want <= 2", n)
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		return os.WriteFile(filePath, []byte("data"), 0600)
	})

	done := make(chan error, 2)
	for i := int64(0); i < 2; i++ {
		media := &MediaInfo{MessageID: i + 1, TDFileID: int32(i + 1), ChatID: 100, MediaType: "photo", FileName: "file_" + string(rune('a'+i)) + ".jpg"}
		go func() { done <- d.DownloadMedia(context.Background(), media) }()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("expected two downloads to start with concurrency=2")
		}
	}
	if got := d.ActiveCount(); got != 2 {
		t.Fatalf("ActiveCount() = %d, want 2", got)
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("DownloadMedia() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("download did not finish")
		}
	}
}

// TestDownloader_PauseAllAndResumeAll 覆盖全局暂停闸：一个下载中、一个排队中的媒体
// 全部暂停（pauseFunc 仅对下载中的调用一次）；闸门置位期间新提交的媒体出生即暂停；
// ResumeAll 后三者全部完成。
func TestDownloader_PauseAllAndResumeAll(t *testing.T) {
	dir := t.TempDir()
	d := New(dir, 1, logger.New(logger.LevelError))

	var pauseCalls, firstCalls int32
	var pauseOnce sync.Once
	pauseSignal := make(chan struct{})
	firstStarted := make(chan struct{})
	thirdRan := make(chan struct{}, 1)

	d.SetPauseFunc(func(_ context.Context, _ *MediaInfo) error {
		atomic.AddInt32(&pauseCalls, 1)
		pauseOnce.Do(func() { close(pauseSignal) })
		return nil
	})
	d.SetDownloadFunc(func(_ context.Context, m *MediaInfo, filePath string) error {
		if m.MessageID == 1 && atomic.AddInt32(&firstCalls, 1) == 1 {
			close(firstStarted)
			<-pauseSignal // 占住唯一槽位直到全局暂停触发底层取消
			return context.Canceled
		}
		if m.MessageID == 3 {
			select {
			case thirdRan <- struct{}{}:
			default:
			}
		}
		return os.WriteFile(filePath, []byte("data"), 0600)
	})

	first := &MediaInfo{TaskID: "t", MessageID: 1, TDFileID: 1, ChatID: 100, MediaType: "photo", FileName: "1.jpg"}
	second := &MediaInfo{TaskID: "t", MessageID: 2, TDFileID: 2, ChatID: 100, MediaType: "photo", FileName: "2.jpg"}
	third := &MediaInfo{TaskID: "t", MessageID: 3, TDFileID: 3, ChatID: 100, MediaType: "photo", FileName: "3.jpg"}

	done1 := make(chan error, 1)
	go func() { done1 <- d.DownloadMedia(context.Background(), first) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first download did not start")
	}

	done2 := make(chan error, 1)
	go func() { done2 <- d.DownloadMedia(context.Background(), second) }()
	waitForStatus(t, d, mediaProgressKey(second), "queued")

	d.PauseAll(context.Background())
	waitForStatus(t, d, mediaProgressKey(first), "paused")
	waitForStatus(t, d, mediaProgressKey(second), "paused")
	if got := atomic.LoadInt32(&pauseCalls); got != 1 {
		t.Fatalf("pauseFunc calls = %d, want 1 (仅下载中的媒体)", got)
	}
	if !d.AllPaused() {
		t.Fatal("AllPaused() = false, want true")
	}

	// 闸门置位期间提交的新媒体出生即暂停，且不执行下载
	done3 := make(chan error, 1)
	go func() { done3 <- d.DownloadMedia(context.Background(), third) }()
	waitForStatus(t, d, mediaProgressKey(third), "paused")
	select {
	case <-thirdRan:
		t.Fatal("全局暂停期间新媒体仍执行了下载")
	default:
	}

	d.ResumeAll()
	if d.AllPaused() {
		t.Fatal("AllPaused() = true after ResumeAll, want false")
	}
	for i, done := range []chan error{done1, done2, done3} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("download %d error = %v", i+1, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("download %d did not finish after ResumeAll", i+1)
		}
	}
	if items := d.ActiveMedia(); len(items) != 0 {
		t.Fatalf("ActiveMedia() after finish = %+v, want empty", items)
	}
}

// TestDownloader_SpeedTracking 直接驱动速率统计：正增量计速、回退只重置基线、窗口过期归零
func TestDownloader_SpeedTracking(t *testing.T) {
	d := newTestDownloader(t.TempDir())
	t0 := time.Now()
	mb := int64(1 << 20)

	d.noteFileBytes(1, mb, t0)
	d.noteFileBytes(1, 3*mb, t0.Add(time.Second))
	if got, want := d.speedAt(t0.Add(time.Second)), 2*mb; got != want {
		t.Fatalf("speedAt(+1s) = %d, want %d", got, want)
	}

	// 字节数回退（重新下载）：不计负增量，速度只随时间摊薄
	d.noteFileBytes(1, mb, t0.Add(1500*time.Millisecond))
	if got, want := d.speedAt(t0.Add(1500*time.Millisecond)), int64(float64(2*mb)/1.5); got != want {
		t.Fatalf("speedAt(+1.5s) after regress = %d, want %d", got, want)
	}

	// 回退后的再增长以新基线计增量
	d.noteFileBytes(1, 2*mb, t0.Add(2*time.Second))
	if got, want := d.speedAt(t0.Add(2*time.Second)), int64(float64(3*mb)/2); got != want {
		t.Fatalf("speedAt(+2s) = %d, want %d", got, want)
	}

	// 窗口过期后无样本，速度归零
	if got := d.speedAt(t0.Add(20 * time.Second)); got != 0 {
		t.Fatalf("speedAt(+20s) = %d, want 0", got)
	}
}

func TestDownloader_PauseAndResumeMedia(t *testing.T) {
	dir := t.TempDir()
	d := New(dir, 1, logger.New(logger.LevelError))
	started := make(chan struct{}, 2)
	pauseRequested := make(chan struct{})
	firstCallReleased := make(chan struct{})
	var calls int32

	d.SetPauseFunc(func(_ context.Context, _ *MediaInfo) error {
		close(pauseRequested)
		return nil
	})
	d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, filePath string) error {
		call := atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		if call == 1 {
			<-pauseRequested
			close(firstCallReleased)
			return context.Canceled
		}
		return os.WriteFile(filePath, []byte("data"), 0600)
	})

	media := &MediaInfo{TaskID: "task-1", MessageID: 1, TDFileID: 2, ChatID: 100, MediaType: "photo", FileName: "pause.jpg"}
	done := make(chan error, 1)
	go func() { done <- d.DownloadMedia(context.Background(), media) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	id := mediaProgressKey(media)
	if err := d.PauseMedia(context.Background(), id); err != nil {
		t.Fatalf("PauseMedia() error = %v", err)
	}
	select {
	case <-firstCallReleased:
	case <-time.After(time.Second):
		t.Fatal("paused download did not release first call")
	}
	items := d.ActiveMedia()
	if len(items) != 1 || items[0].Status != "paused" {
		t.Fatalf("ActiveMedia() after pause = %+v, want one paused item", items)
	}
	if err := d.ResumeMedia(id); err != nil {
		t.Fatalf("ResumeMedia() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not restart after resume")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DownloadMedia() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download did not finish")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("download calls = %d, want 2", got)
	}
	if items := d.ActiveMedia(); len(items) != 0 {
		t.Fatalf("ActiveMedia() after finish = %+v, want empty", items)
	}
}
