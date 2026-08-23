package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
)

type fakeDownloadLifecycleAPI struct {
	tdAPI
	downloadStarted chan struct{}
	downloadRelease chan struct{}
	startOnce       sync.Once
	releaseOnce     sync.Once
	downloadCalls   atomic.Int32
	cancelCalls     atomic.Int32
	closeCalls      atomic.Int32
	closeBlock      <-chan struct{}
}

func newFakeDownloadLifecycleAPI() *fakeDownloadLifecycleAPI {
	return &fakeDownloadLifecycleAPI{
		downloadStarted: make(chan struct{}),
		downloadRelease: make(chan struct{}),
	}
}

func (f *fakeDownloadLifecycleAPI) DownloadFile(
	_ context.Context, req *tdclient.DownloadFileRequest,
) (*tdclient.File, error) {
	f.downloadCalls.Add(1)
	f.startOnce.Do(func() { close(f.downloadStarted) })
	<-f.downloadRelease
	return &tdclient.File{Id: req.FileId, Local: &tdclient.LocalFile{}}, nil
}

func (f *fakeDownloadLifecycleAPI) CancelDownloadFile(
	_ context.Context, _ *tdclient.CancelDownloadFileRequest,
) (*tdclient.Ok, error) {
	f.cancelCalls.Add(1)
	f.releaseOnce.Do(func() { close(f.downloadRelease) })
	return &tdclient.Ok{}, nil
}

func (f *fakeDownloadLifecycleAPI) Close(_ context.Context) (*tdclient.Ok, error) {
	f.closeCalls.Add(1)
	if f.closeBlock != nil {
		<-f.closeBlock
	}
	return &tdclient.Ok{}, nil
}

func TestPauseDownloadCancelsInternalRetrier(t *testing.T) {
	c := newTestClient(t)
	fake := newFakeDownloadLifecycleAPI()
	c.mu.Lock()
	c.td = fake
	c.mu.Unlock()

	media := &downloader.MediaInfo{TDFileID: 7, FileName: "paused.bin", FileSize: 1}
	done := make(chan error, 1)
	go func() {
		done <- c.DownloadFile(context.Background(), media, filepath.Join(t.TempDir(), "paused.bin"))
	}()
	select {
	case <-fake.downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("下载未开始")
	}
	if err := c.pauseDownloadFile(context.Background(), media); err != nil {
		t.Fatalf("pauseDownloadFile() error = %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DownloadFile() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("暂停后下载没有退出")
	}
	if calls := fake.downloadCalls.Load(); calls != 1 {
		t.Fatalf("暂停后 TDLib 下载被重试，调用次数 = %d", calls)
	}
}

func TestStoppingMonitorCancelsAndWaitsForDownloads(t *testing.T) {
	c := newTestClient(t)
	fake := newFakeDownloadLifecycleAPI()
	c.mu.Lock()
	c.td = fake
	c.mu.Unlock()
	c.SetMonitorTask("monitor", 100, "chat")

	c.onNewMessage(&tdclient.Message{
		Id: 1, ChatId: 100,
		Content: &tdclient.MessageDocument{Document: &tdclient.Document{
			FileName: "live.bin", Document: &tdclient.File{Id: 9, Size: 1},
		}},
	})
	select {
	case <-fake.downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("实时监控下载未开始")
	}

	done := make(chan struct{})
	go func() {
		c.SetMonitorTask("", 0, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("停止监控未等待下载取消")
	}
	if fake.cancelCalls.Load() == 0 {
		t.Fatal("停止监控未调用 CancelDownloadFile")
	}
	if c.TargetChat() != 0 {
		t.Fatalf("停止后的监控目标 = %d, want 0", c.TargetChat())
	}
}

func TestCloseTDClientHasTimeout(t *testing.T) {
	release := make(chan struct{})
	fake := newFakeDownloadLifecycleAPI()
	fake.closeBlock = release
	started := time.Now()
	err := closeTDClient(fake, 20*time.Millisecond)
	close(release)
	if err == nil {
		t.Fatal("阻塞的 TDLib Close 应返回超时")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Close 超时耗时 %v，未受到上限约束", elapsed)
	}
}

func TestClientCloseIsRepeatSafe(t *testing.T) {
	c := newTestClient(t)
	fake := newFakeDownloadLifecycleAPI()
	fake.releaseOnce.Do(func() { close(fake.downloadRelease) })
	c.mu.Lock()
	c.td = fake
	c.mu.Unlock()
	c.Close()
	c.Close()
	if calls := fake.closeCalls.Load(); calls != 1 {
		t.Fatalf("TDLib Close 调用次数 = %d, want 1", calls)
	}
}

func TestEnsurePrivateDirTightensExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不提供相同的 Unix 权限语义")
	}
	dir := filepath.Join(t.TempDir(), "tdlib")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != dbDirPerm {
		t.Fatalf("TDLib 目录权限 = %o, want %o", info.Mode().Perm(), dbDirPerm)
	}
}

func TestRemoveSessionDatabaseRejectsSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		linkRoot     bool
		linkAncestor bool
	}{
		{name: "session root symlink", linkRoot: true},
		{name: "tdlib symlink"},
		{name: "ancestor symlink", linkAncestor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			outsideRoot := t.TempDir()
			outsideDB := filepath.Join(outsideRoot, "tdlib")
			if tc.linkAncestor {
				outsideDB = filepath.Join(outsideRoot, "sessions", "tdlib")
			}
			if err := os.MkdirAll(outsideDB, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(outsideDB, "db.bin")
			if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}

			sessionDir := filepath.Join(base, "sessions")
			if tc.linkAncestor {
				link := filepath.Join(base, "linked-parent")
				if err := os.Symlink(outsideRoot, link); err != nil {
					t.Skipf("无法创建符号链接: %v", err)
				}
				sessionDir = filepath.Join(link, "sessions")
			} else if tc.linkRoot {
				if err := os.Symlink(outsideRoot, sessionDir); err != nil {
					t.Skipf("无法创建符号链接: %v", err)
				}
			} else {
				if err := os.Mkdir(sessionDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideDB, filepath.Join(sessionDir, "tdlib")); err != nil {
					t.Skipf("无法创建符号链接: %v", err)
				}
			}

			err := removeSessionDatabase(sessionDir, filepath.Join(sessionDir, "tdlib"))
			if err == nil {
				t.Fatal("符号链接会话路径应被拒绝")
			}
			got, readErr := os.ReadFile(marker)
			if readErr != nil || string(got) != "keep" {
				t.Fatalf("外部会话文件被删除或修改: %q, %v", got, readErr)
			}
		})
	}
}

func TestClientCredentialValidationUsesConfigRules(t *testing.T) {
	c := newTestClient(t)
	validHash := "0123456789abcdef0123456789abcdef"
	c.SetCredentials(12345, validHash, "+12025550123")
	if !c.HasCredentials() {
		t.Fatal("合法凭据被拒绝")
	}

	for _, invalidID := range []int{-1, 0, int(config.MaxTelegramAPIID) + 1} {
		c.SetCredentials(invalidID, validHash, "+12025550123")
		if c.HasCredentials() {
			t.Errorf("非法 API ID %d 被 HasCredentials 接受", invalidID)
		}
	}

	c.SetCredentials(-1, validHash, "+12025550123")
	err := c.Connect(context.Background(), nil, nil)
	if err == nil || err.Error() != "无效的 Telegram API 凭据" {
		t.Fatalf("Connect() error = %v, want 凭据无效", err)
	}
	if _, statErr := os.Stat(c.dbDir); !os.IsNotExist(statErr) {
		t.Fatalf("非法凭据不应创建 TDLib 目录: %v", statErr)
	}
}
