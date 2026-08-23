// 桌面客户端入口：以内嵌引擎（internal/telegram + internal/web）为后端，
// Wails WebView 为界面，壳层服务（internal/desktop）负责实例反代与系统集成。
//
// 构建：make build-desktop（三平台各自原生编译，与服务器版同一 TDLib 链接要求）。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/gen2brain/beeep"

	"tg-down/internal/config"
	"tg-down/internal/desktop"
	"tg-down/internal/logger"
	"tg-down/internal/store"
	"tg-down/internal/telegram"
	"tg-down/internal/web"
)

// version 由构建时 -ldflags "-X main.version=..." 注入
var version = "dev"

// trayDisableEnv 设为 0/1 之外的任意非空值时禁用托盘（部分 Wayland 会话的兜底开关）
const trayDisableEnv = "TG_DOWN_DESKTOP_NO_TRAY"

func main() {
	noTray := flag.Bool("no-tray", false, "禁用系统托盘（调试用）")
	flag.Parse()

	if err := run(!*noTray); err != nil {
		fmt.Fprintf(os.Stderr, "[桌面端] %v\n", err)
		os.Exit(1)
	}
}

func run(enableTray bool) error {
	appDir, err := desktop.AppDir()
	if err != nil {
		return err
	}

	// 引擎生命周期上下文：随窗口退出取消，带动 Telegram 连接、队列、HTTP 全部收尾
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eng, err := startEngine(ctx)
	if err != nil {
		return fmt.Errorf("启动本地下载引擎失败: %w", err)
	}
	defer eng.Close()

	reg, err := desktop.OpenRegistry(appDir + "/" + desktop.InstancesFile)
	if err != nil {
		return fmt.Errorf("加载远程实例注册表失败: %w", err)
	}

	autostart := desktop.NewAutostart()
	shell := desktop.NewShell(reg, eng.Base(), appDir, version, autostart)
	shellURL, err := shell.Start(ctx)
	if err != nil {
		return fmt.Errorf("启动壳服务失败: %w", err)
	}
	defer shell.Stop()

	// 本地任务失败的系统通知（远程实例由其自身 notify 配置负责）
	go desktop.WatchTaskFailures(ctx, eng.Base(), eng.log)

	go checkUpdateOnce(eng.log)

	ui := &uiApp{shellURL: shellURL, engineBase: eng.Base()}
	if enableTray || os.Getenv(trayDisableEnv) == "" && false { // 占位，下一行覆盖
		_ = ui
	}
	if enableTray && os.Getenv(trayDisableEnv) == "" {
		ui.startTrayAsync(autostart)
	}

	if err := wailsRun(buildOptions(ui)); err != nil {
		return fmt.Errorf("窗口运行失败: %w", err)
	}
	cancel() // 窗口退出后触发引擎收尾
	eng.Wait()
	eng.log.Info("桌面客户端已退出")
	return nil
}

// --- 引擎托管 ---

type engineHandle struct {
	base string
	srv  *web.Server
	// client 持引用防止 GC；连接关闭由 web.Server 在 ctx 路径完成
	client *telegram.Client
	st     *store.Store
	done   chan struct{}
	log    *logger.Logger
}

// Base 返回引擎根地址（形如 http://127.0.0.1:<port>）
func (e *engineHandle) Base() string { return e.base }

// Wait 阻塞直到引擎 HTTP 服务完全退出
func (e *engineHandle) Wait() { <-e.done }

// Close 幂等清理存储
func (e *engineHandle) Close() {
	if e.st != nil {
		_ = e.st.Close()
		e.st = nil
	}
}

// startEngine 复刻 --web 模式的装配链，但监听回环随机端口且不注册信号处理：
// 生命周期完全由窗口/托盘驱动。
func startEngine(ctx context.Context) (*engineHandle, error) {
	cfg, err := config.LoadConfigForWeb()
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}
	log := logger.New(cfg.Log.Level)
	// 桌面端默认开启文件日志（应用数据目录 logs/tg-down.log），便于排查崩溃；
	// 用户显式配置过 log.file 则尊重原值
	logFile := cfg.Log.File
	if logFile == "" {
		if logsDir, lerr := desktop.LogsDir(); lerr == nil {
			logFile = logsDir + "/tg-down.log"
		}
	}
	if logFile != "" {
		if serr := log.SetFileSink(logFile, logger.DefaultFileLogMaxBytes, logger.DefaultFileLogKeep); serr != nil {
			log.Warn("日志文件不可用，仅输出到控制台: %v", serr)
		}
	}

	log.Info("Tg-Down 桌面客户端 %s 启动", version)
	client := telegram.NewWithUpdates(cfg, log, 0)
	if client == nil {
		return nil, fmt.Errorf("创建客户端失败")
	}
	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	srv := web.New(client, st, log, "127.0.0.1:0", cfg)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("绑定引擎端口失败: %w", err)
	}
	h := &engineHandle{
		base: "http://" + ln.Addr().String(),
		srv:  srv,
		log:  log,
		done: make(chan struct{}),
	}
	h.client = client
	h.st = st
	go func() {
		defer close(h.done)
		_ = h.srv.Serve(ctx, ln)
	}()
	return h, nil
}

// checkUpdateOnce 单次更新检查并弹系统通知
func checkUpdateOnce(log *logger.Logger) {
	rel, err := desktop.CheckUpdate(version)
	if err != nil {
		log.Debug("更新检查跳过: %v", err)
		return
	}
	if rel == nil {
		return
	}
	msg := "发现新版本 " + rel.Tag + "，请到发布页下载"
	log.Info("%s：%s", msg, rel.URL)
	_ = beeep.Notify(desktop.AppName, msg, "")
}
