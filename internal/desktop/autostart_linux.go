//go:build linux

package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Linux 实现：XDG 自启目录下的 .desktop 文件（GNOME/KDE/多数桌面环境通用）

const linuxEntryName = "tg-down.desktop"

type linuxAutostart struct{}

// NewAutostart 返回当前平台的实现
func NewAutostart() Autostart { return linuxAutostart{} }

func (linuxAutostart) entryPath() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("解析用户配置目录失败: %w", err)
	}
	return filepath.Join(cfg, "autostart", linuxEntryName), nil
}

func (linuxAutostart) Enabled() (bool, error) {
	p, err := (linuxAutostart{}).entryPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // p 由 entryPath 从用户配置目录拼出，非外部输入
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !strings.Contains(string(data), "Hidden=true"), nil
}

func (linuxAutostart) Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	p, err := (linuxAutostart{}).entryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("创建 autostart 目录失败: %w", err)
	}
	content := `[Desktop Entry]
Type=Application
Name=Tg-Down
Exec=` + exe + `
Terminal=false
X-GNOME-Autostart-enabled=true
Categories=Network;
`
	return os.WriteFile(p, []byte(content), 0o600)
}

func (linuxAutostart) Disable() error {
	p, err := (linuxAutostart{}).entryPath()
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
