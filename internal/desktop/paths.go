// Package desktop 提供 Tg-Down 桌面客户端的外壳能力：应用数据目录管理、本地下载引擎托管、
// 远程实例反代与注册表、托盘、开机自启、系统通知与更新检查。
//
// 分层约束：本包不 import internal/telegram（CGo 边界守卫），引擎复用经由 internal/web
// 的导出接口（Serve/UIHandler）完成。
package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AppName 决定各平台的目录名与托盘标题
const AppName = "Tg-Down"

// DirOverrideEnv 允许开发或测试时把应用数据目录指到别处（避免污染真实用户目录）
const DirOverrideEnv = "TG_DOWN_DESKTOP_DIR"

// AppDir 返回并确保应用数据目录存在，随后把进程工作目录切换过去。
// 引擎的全部相对路径配置（downloads/、sessions/、config.yaml、tg-down.db）因此天然落位：
//
//	macOS:   ~/Library/Application Support/Tg-Down
//	Windows: %AppData%\Tg-Down
//	Linux:   $XDG_CONFIG_HOME/tg-down 或 ~/.config/tg-down
func AppDir() (string, error) {
	dir := os.Getenv(DirOverrideEnv)
	if dir == "" {
		switch runtime.GOOS {
		case "darwin":
			base, e := os.UserHomeDir()
			if e != nil {
				return "", fmt.Errorf("解析用户主目录失败: %w", e)
			}
			dir = filepath.Join(base, "Library", "Application Support", AppName)
		default:
			base, e := os.UserConfigDir()
			if e != nil {
				return "", fmt.Errorf("解析用户配置目录失败: %w", e)
			}
			dir = filepath.Join(base, lowerName())
		}
	}
	// dir 只可能是平台标准目录，或开发者自己经 TG_DOWN_DESKTOP_DIR 指定的路径；
	// 能设置本进程环境变量的一方已经控制了本进程，不构成越权路径。
	//nolint:gosec // 路径来自本进程环境变量，非远程输入
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("创建应用数据目录失败: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return "", fmt.Errorf("切换工作目录到 %s 失败: %w", dir, err)
	}
	return dir, nil
}

// LogsDir 返回日志目录（位于应用数据目录下），不存在则创建
func LogsDir() (string, error) {
	const dir = "logs"
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("创建日志目录失败: %w", err)
	}
	return filepath.Abs(dir)
}

// InstancesFile 返回远程实例注册表文件的路径（相对应用数据目录，0600 权限存储令牌）
const InstancesFile = "instances.json"

func lowerName() string {
	n := AppName
	if runtime.GOOS == "linux" {
		// Linux 惯例用小写目录名
		b := []byte(n)
		for i := range b {
			if b[i] >= 'A' && b[i] <= 'Z' {
				b[i] += 'a' - 'A'
			}
		}
		n = string(b)
	}
	return n
}
