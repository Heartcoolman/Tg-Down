package main

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"tg-down/internal/desktop"
)

// wailsRun 间接引用 wails.Run，便于测试替换
func wailsRun(opts *options.App) error { return wails.Run(opts) }

// uiApp 承载 Wails 生命周期回调与托盘动作
type uiApp struct {
	ctx        atomic.Pointer[context.Context]
	shellURL   string
	engineBase string
}

// storeCtx 在 OnStartup 里记录运行时上下文；托盘动作据此操作窗口
func (a *uiApp) startup(ctx context.Context) {
	a.ctx.Store(&ctx)
}

func (a *uiApp) show() {
	if ctx := a.ctx.Load(); ctx != nil {
		runtime.WindowUnminimise(*ctx)
		runtime.WindowShow(*ctx)
	}
}

func (a *uiApp) quit() {
	if ctx := a.ctx.Load(); ctx != nil {
		runtime.Quit(*ctx)
	}
}

// openExternal 用系统浏览器打开链接（发布页等）
func (a *uiApp) openExternal(url string) {
	if ctx := a.ctx.Load(); ctx != nil {
		runtime.BrowserOpenURL(*ctx, url)
	}
}

// buildOptions 组装 Wails 应用配置。
//
// 窗口初始加载的是壳服务的引导页：一段立即 location.replace 到 shellURL 的极简 HTML。
// 之后 WebView 的地址即本地壳服务，SPA 的同源相对路径、SSE、Cookie 全部天然成立，
// 不依赖任何平台特定的"导航到 URL"运行时接口。
func buildOptions(a *uiApp) *options.App {
	return &options.App{
		Title:             desktop.AppName,
		Width:             1280,
		Height:            800,
		MinWidth:          960,
		MinHeight:         600,
		HideWindowOnClose: true, // 关闭即隐藏到托盘，引擎保持运行
		AssetServer: &assetserver.Options{
			Handler: bootstrapHandler(a.shellURL),
		},
		OnStartup: a.startup,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "app.tg-down.desktop",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { a.show() },
		},
		Mac: &mac.Options{
			About: &mac.AboutInfo{
				Title:   desktop.AppName,
				Message: "Telegram 媒体下载器 · 桌面客户端 " + version,
			},
		},
		Windows: &windows.Options{},
	}
}

// bootstrapHandler 输出跳转引导页。仅被看到一次；随后 WebView 已在壳服务地址上。
func bootstrapHandler(target string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Tg-Down</title>
<body style="font-family:system-ui;background:#f2f2f7;color:#333;text-align:center;padding-top:40vh">正在启动 Tg-Down…</body>
<script>location.replace(%q)</script>`, target)
	})
}
