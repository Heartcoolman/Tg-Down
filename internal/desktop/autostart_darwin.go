package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// macOS 实现：LaunchAgent plist + launchctl。KeepAlive 不设——自启只负责登录时拉起，
// 退出后不自动复活（用户可从托盘手动再开）。

const (
	darwinPlistName = "app.tg-down.desktop.plist"
	// launchctlTimeout 给 launchctl 一个上限：它偶发挂起时不应拖住调用它的界面线程
	launchctlTimeout = 10 * time.Second
)

type darwinAutostart struct{}

func (d darwinAutostart) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户主目录失败: %w", err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	return filepath.Join(dir, darwinPlistName), nil
}

// NewAutostart 返回当前平台的实现
func NewAutostart() Autostart { return darwinAutostart{} }

func (darwinAutostart) Enabled() (bool, error) {
	p, err := (darwinAutostart{}).plistPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (darwinAutostart) Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("解析可执行文件路径失败: %w", err)
	}
	p, err := (darwinAutostart{}).plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	label := "app.tg-down.desktop"
	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>` + label + `</string>
	<key>ProgramArguments</key><array><string>` + exe + `</string></array>
	<key>RunAtLoad</key><true/>
</dict></plist>
` + ""
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入 LaunchAgent 失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()
	// 已加载过的 agent 重复 load 会报错，属预期，忽略
	_ = exec.CommandContext(ctx, "launchctl", "load", "-w", p).Run() //nolint:gosec // 固定路径与参数
	return nil
}

func (darwinAutostart) Disable() error {
	p, err := (darwinAutostart{}).plistPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()
	_ = exec.CommandContext(ctx, "launchctl", "unload", "-w", p).Run() //nolint:gosec // 同上
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
