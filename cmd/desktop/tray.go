package main

import (
	"bytes"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/energye/systray"
	"github.com/gen2brain/beeep"

	"tg-down/internal/desktop"
)

// startTrayAsync 注册系统托盘。systray.Register 非阻塞，专为内嵌进已有事件循环
// （这里是 Wails）的应用设计；菜单点击经回调分发。
func (a *uiApp) startTrayAsync(autostart desktop.Autostart) {
	systray.Register(func() { a.onTrayReady(autostart) }, func() {})
}

func (a *uiApp) onTrayReady(autostart desktop.Autostart) {
	if runtime.GOOS == "windows" {
		systray.SetIcon(desktop.TrayIconICO())
	} else {
		systray.SetIcon(desktop.TrayIconPNG())
	}
	systray.SetTooltip(desktop.AppName + " " + version)

	mShow := systray.AddMenuItem("显示主窗口", "打开管理窗口")
	systray.AddSeparator()
	mPause := systray.AddMenuItem("暂停全部下载", "暂停本地引擎的全部媒体下载")
	mResume := systray.AddMenuItem("恢复全部下载", "恢复本地引擎的媒体下载")
	systray.AddSeparator()
	enabled, _ := autostart.Enabled()
	mAuto := systray.AddMenuItemCheckbox("开机自启", "登录系统时自动启动 Tg-Down", enabled)
	mUpdate := systray.AddMenuItem("检查更新…", "查询 GitHub 最新发布")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "结束引擎并退出")

	mShow.Click(a.show)
	mPause.Click(func() { a.enginePost("/api/media/pause-all") })
	mResume.Click(func() { a.enginePost("/api/media/resume-all") })
	mUpdate.Click(a.checkUpdateInteractive)
	mQuit.Click(a.quit)
	mAuto.Click(func() {
		if on, _ := autostart.Enabled(); on {
			if err := autostart.Disable(); err == nil {
				mAuto.Uncheck()
			}
		} else if err := autostart.Enable(); err == nil {
			mAuto.Check()
		} else {
			_ = beeep.Notify(desktop.AppName, "设置开机自启失败: "+err.Error(), "")
		}
	})
}

// enginePost 调用本地引擎的控制端点（回环无鉴权）
func (a *uiApp) enginePost(path string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(a.engineBase+path, "application/json", bytes.NewReader(nil)) //nolint:noctx // 固定拼接的回环地址
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func (a *uiApp) checkUpdateInteractive() {
	go func() {
		rel, err := desktop.CheckUpdate(version)
		switch {
		case err != nil:
			_ = beeep.Notify(desktop.AppName, fmt.Sprintf("更新检查失败: %v", err), "")
		case rel == nil:
			_ = beeep.Notify(desktop.AppName, "当前已是最新版本 "+version, "")
		default:
			_ = beeep.Notify(desktop.AppName, "发现新版本 "+rel.Tag+"，正在打开发布页…", "")
			a.openExternal(rel.URL)
		}
	}()
}
