//go:build windows

package desktop

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// Windows 实现：HKCU\...\Run 注册表键，值为带引号的完整可执行路径

const winRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const winValueName = "Tg-Down"

type windowsAutostart struct{}

// NewAutostart 返回当前平台的实现
func NewAutostart() Autostart { return windowsAutostart{} }

func (windowsAutostart) Enabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(winValueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v != "", nil
}

func (windowsAutostart) Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue(winValueName, `"`+exe+`"`)
}

func (windowsAutostart) Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer func() { _ = k.Close() }()
	err = k.DeleteValue(winValueName)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}
