package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// 文件日志默认参数：10MB 轮转，保留 3 个历史文件
const (
	DefaultFileLogMaxBytes = 10 << 20
	DefaultFileLogKeep     = 3
)

// fileSink 带大小轮转的追加写文件。轮转策略：当前写满后整体改名 .1，
// 旧的 .1→.2 依次顺延，超出保留数的最老文件删除。无定时器，纯写入时检查。
type fileSink struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	max     int64
	keep    int
	written int64
	rotFail sync.Once
}

func newFileSink(path string, maxBytes int64, keep int) (*fileSink, error) {
	if path == "" {
		return nil, fmt.Errorf("日志文件路径为空")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultFileLogMaxBytes
	}
	if keep <= 0 {
		keep = DefaultFileLogKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	//nolint:gosec // path 来自本地配置 log.file，非外部请求输入
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("读取日志文件大小失败: %w", err)
	}
	return &fileSink{f: f, path: path, max: maxBytes, keep: keep, written: info.Size()}, nil
}

// writeLine 追加一行；写失败或轮转失败只静默降级一次提示到 stderr，
// 不能让日志故障反过来打断下载主流程。
func (s *fileSink) writeLine(line string) {
	data := []byte(line + "\n")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	n, err := s.f.Write(data)
	if err == nil {
		s.written += int64(n)
		if s.written >= s.max {
			s.rotateLocked()
		}
		return
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	s.rotFail.Do(func() {
		fmt.Fprintf(os.Stderr, "[日志] 写入日志文件失败，后续仅输出到控制台: %v\n", err)
	})
}

func (s *fileSink) rotateLocked() {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	// 先删最老再逐级顺延：Windows 的 rename 不覆盖已存在目标，必须保证目的地为空。
	_ = os.Remove(rotName(s.path, s.keep))
	for i := s.keep - 1; i >= 1; i-- {
		_ = os.Rename(rotName(s.path, i), rotName(s.path, i+1))
	}
	_ = os.Rename(s.path, rotName(s.path, 1))
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.rotFail.Do(func() {
			fmt.Fprintf(os.Stderr, "[日志] 日志轮转失败，后续仅输出到控制台: %v\n", err)
		})
		return
	}
	s.f = f
	s.written = 0
}

func (s *fileSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
}

func rotName(path string, n int) string {
	return path + "." + fmt.Sprint(n)
}
