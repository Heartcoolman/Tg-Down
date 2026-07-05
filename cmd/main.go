// Package main implements the entry point for the Telegram media downloader application.
// It provides functionality to download media files from Telegram chats and monitor new messages.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/logger"
	"tg-down/internal/store"
	"tg-down/internal/telegram"
	"tg-down/internal/web"
)

const (
	// ModeDownloadHistory is the mode for downloading historical media.
	ModeDownloadHistory = 1
	// ModeMonitorNewMessages is the mode for monitoring new messages.
	ModeMonitorNewMessages = 2
	// ModeDownloadAndMonitor is the mode for both downloading history and monitoring new messages.
	ModeDownloadAndMonitor = 3

	// ExitCodeConfigError is the exit code for configuration errors.
	ExitCodeConfigError = 1
	// ExitCodeChatError is the exit code for chat selection errors.
	ExitCodeChatError = 1
	// ExitCodeClientError is the exit code for client creation errors.
	ExitCodeClientError = 1
	// ExitCodeRunError is the exit code for runtime errors.
	ExitCodeRunError = 1
	// ExitCodeSessionError is the exit code for session errors.
	ExitCodeSessionError = 1

	// SignalBufferSize is the buffer size for signal channel.
	SignalBufferSize = 1

	// MinChatChoice is the minimum valid chat choice.
	MinChatChoice = 1
	// MaxModeChoice is the maximum valid mode choice.
	MaxModeChoice = 3
)

func main() {
	// 清除会话: tg-down --clear-session
	if len(os.Args) > 1 && os.Args[1] == "--clear-session" {
		clearSessionAndExit()
		return
	}

	// Web 管理端模式: tg-down --web [监听地址]
	if len(os.Args) > 1 && os.Args[1] == "--web" {
		addr := web.DefaultAddr
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		runWebMode(addr)
		return
	}

	cfg, log := initializeApplication()

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		log.Error("打开数据库失败: %v", err)
		os.Exit(ExitCodeConfigError)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := setupSignalHandling(log)
	defer cancel()

	mode := selectMode(log)

	// TDLib 客户端始终带更新监听；是否触发实时下载由 targetChatID 控制
	client := telegram.NewWithUpdates(cfg, log, 0)
	client.SetRecordFunc(store.NewRecorder(st))
	log.Info("正在连接到Telegram...")
	if err := client.Authenticate(ctx); err != nil {
		log.Error("连接/认证失败: %v", err)
		os.Exit(ExitCodeRunError)
	}
	defer client.Close()
	log.Info("成功连接到Telegram")

	targetChatID := resolveTargetChat(ctx, cfg, client, log)
	if mode == ModeMonitorNewMessages || mode == ModeDownloadAndMonitor {
		client.SetMonitorTask(fmt.Sprintf("cli-monitor-%d", time.Now().UnixNano()), targetChatID)
	}

	if err := executeMode(ctx, cancel, client, log, mode, targetChatID); err != nil {
		log.Error("运行失败: %v", err)
		os.Exit(ExitCodeRunError)
	}
	log.Info("程序退出")
}

// runWebMode 启动 Web 管理端（允许无凭据启动，登录信息可在网页内填写）
func runWebMode(addr string) {
	cfg, err := config.LoadConfigForWeb()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(ExitCodeConfigError)
	}
	log := logger.New(cfg.Log.Level)
	log.Info("Telegram群聊媒体下载器启动 (Web 模式)")

	client := telegram.NewWithUpdates(cfg, log, 0)
	if client == nil {
		log.Error("创建客户端失败")
		os.Exit(ExitCodeClientError)
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		log.Error("打开数据库失败: %v", err)
		os.Exit(ExitCodeConfigError)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := setupSignalHandling(log)
	defer cancel()

	if err := web.New(client, st, log, addr, cfg.Queue.MaxConcurrentTasks).Run(ctx); err != nil {
		log.Error("Web 服务运行失败: %v", err)
		os.Exit(ExitCodeRunError)
	}
	log.Info("程序退出")
}

// initializeApplication 初始化应用程序配置和日志
func initializeApplication() (*config.Config, *logger.Logger) {
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		fmt.Println("请确保已正确配置 config.yaml 或环境变量")
		fmt.Println("可以参考 config.yaml.example 和 .env.example 文件")
		os.Exit(ExitCodeConfigError)
	}

	log := logger.New(cfg.Log.Level)
	log.Info("Telegram群聊媒体下载器启动")
	return cfg, log
}

// setupSignalHandling 设置信号处理
func setupSignalHandling(log *logger.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, SignalBufferSize)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Info("收到中断信号，正在退出...")
		cancel()
	}()

	return ctx, cancel
}

// resolveTargetChat 决定目标聊天ID：优先用配置，否则交互式选择
func resolveTargetChat(ctx context.Context, cfg *config.Config, client *telegram.Client, log *logger.Logger) int64 {
	if cfg.Chat.TargetID != 0 {
		log.Info("使用配置的聊天ID: %d", cfg.Chat.TargetID)
		return cfg.Chat.TargetID
	}

	chatID, err := selectChat(ctx, client, log)
	if err != nil {
		log.Error("选择聊天失败: %v", err)
		os.Exit(ExitCodeChatError)
	}
	return chatID
}

// executeMode 执行指定的操作模式
func executeMode(
	ctx context.Context,
	cancel context.CancelFunc,
	client *telegram.Client,
	log *logger.Logger,
	mode int,
	targetChatID int64,
) error {
	switch mode {
	case ModeDownloadHistory:
		return executeDownloadHistory(ctx, client, log, targetChatID)
	case ModeMonitorNewMessages:
		return executeMonitorNewMessages(ctx, cancel, client, log, targetChatID)
	case ModeDownloadAndMonitor:
		return executeDownloadAndMonitor(ctx, client, log, targetChatID)
	default:
		return fmt.Errorf("未知的操作模式: %d", mode)
	}
}

// executeDownloadHistory 执行下载历史媒体模式
func executeDownloadHistory(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64) error {
	log.Info("开始下载历史媒体文件...")
	taskID := fmt.Sprintf("cli-history-%d", time.Now().UnixNano())
	if err := client.DownloadHistoryMedia(ctx, targetChatID, taskID); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("下载历史媒体失败: %w", err)
	}
	return nil
}

// executeMonitorNewMessages 执行监控新消息模式
func executeMonitorNewMessages(
	ctx context.Context,
	cancel context.CancelFunc,
	client *telegram.Client,
	log *logger.Logger,
	targetChatID int64,
) error {
	log.Info("开始实时监控新消息...")
	log.Info("实时监控已启动，目标聊天ID: %d", targetChatID)

	startInteractiveMonitoring(ctx, cancel, client, log, targetChatID)
	<-ctx.Done()
	return nil
}

// executeDownloadAndMonitor 执行下载历史并监控新消息模式
func executeDownloadAndMonitor(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64) error {
	log.Info("开始下载历史媒体文件...")
	taskID := fmt.Sprintf("cli-history-%d", time.Now().UnixNano())
	if err := client.DownloadHistoryMedia(ctx, targetChatID, taskID); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("下载历史媒体失败: %w", err)
	}
	log.Info("历史媒体下载完成，实时监控已自动启动")
	log.Info("实时监控已启动，目标聊天ID: %d", targetChatID)
	<-ctx.Done()
	return nil
}

// startInteractiveMonitoring 启动交互式监控
func startInteractiveMonitoring(
	ctx context.Context,
	cancel context.CancelFunc,
	client *telegram.Client,
	log *logger.Logger,
	targetChatID int64,
) {
	go func() {
		fmt.Println("\n监控已启动！")
		fmt.Println("输入命令:")
		fmt.Println("  'check' - 手动检查新消息")
		fmt.Println("  'status' - 查看监控状态")
		fmt.Println("  'quit' - 退出程序")
		fmt.Print("> ")

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var input string
			if _, scanErr := fmt.Scanln(&input); scanErr != nil {
				continue
			}

			if !handleInteractiveCommand(ctx, cancel, client, log, targetChatID, input) {
				return
			}
			fmt.Print("> ")
		}
	}()
}

// handleInteractiveCommand 处理交互式命令
func handleInteractiveCommand(
	ctx context.Context,
	cancel context.CancelFunc,
	client *telegram.Client,
	log *logger.Logger,
	targetChatID int64,
	input string,
) bool {
	switch input {
	case "check":
		log.Info("手动检查新消息...")
		if err := client.ManualCheckNewMessages(ctx, targetChatID); err != nil {
			log.Error("手动检查失败: %v", err)
		}
	case "status":
		log.Info("监控状态: 正在运行，目标聊天ID: %d", targetChatID)
	case "quit":
		log.Info("用户请求退出")
		cancel()
		return false
	default:
		fmt.Println("未知命令，请输入 'check', 'status' 或 'quit'")
	}
	return true
}

// selectChat 选择目标聊天
func selectChat(ctx context.Context, client *telegram.Client, log *logger.Logger) (int64, error) {
	log.Info("获取聊天列表...")
	chats, err := client.GetChats(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取聊天列表失败: %w", err)
	}

	if len(chats) == 0 {
		return 0, fmt.Errorf("没有找到任何聊天")
	}

	displayChatList(chats)
	choice := getUserChatChoice(log, len(chats))

	selectedChat := chats[choice-1]
	log.Info("选择了聊天: %s (ID: %d)", selectedChat.Title, selectedChat.ID)
	return selectedChat.ID, nil
}

// displayChatList 显示聊天列表
func displayChatList(chats []telegram.ChatInfo) {
	fmt.Println("\n可用的聊天:")
	for i, chat := range chats {
		fmt.Printf("%d. %s (%s) - ID: %d\n", i+1, chat.Title, chat.Type, chat.ID)
	}
}

// getUserChatChoice 获取用户的聊天选择
func getUserChatChoice(log *logger.Logger, maxChoice int) int {
	fmt.Print("\n请选择聊天 (输入序号): ")
	var choice int
	if _, err := fmt.Scanln(&choice); err != nil {
		log.Warn("读取输入失败: %v", err)
		os.Exit(ExitCodeChatError)
	}

	if choice < MinChatChoice || choice > maxChoice {
		log.Error("选择无效")
		os.Exit(ExitCodeChatError)
	}

	return choice
}

// selectMode 选择操作模式
func selectMode(log *logger.Logger) int {
	fmt.Println("\n请选择操作模式:")
	fmt.Println("1. 只下载历史媒体文件")
	fmt.Println("2. 只监控新消息")
	fmt.Println("3. 下载历史媒体文件 + 监控新消息")

	fmt.Print("\n请选择模式 (1-3): ")
	var choice string
	if _, err := fmt.Scanln(&choice); err != nil {
		log.Warn("读取输入失败，使用默认模式 %d", ModeDownloadAndMonitor)
		return ModeDownloadAndMonitor
	}

	mode, err := strconv.Atoi(choice)
	if err != nil || mode < ModeDownloadHistory || mode > MaxModeChoice {
		log.Warn("输入无效，使用默认模式 %d", ModeDownloadAndMonitor)
		return ModeDownloadAndMonitor
	}

	return mode
}

// clearSessionAndExit 清除会话并退出
func clearSessionAndExit() {
	fmt.Println("正在清除会话...")

	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(ExitCodeConfigError)
	}

	log := logger.New(config.DefaultLogLevel)
	client := telegram.New(cfg, log)

	if err := client.ClearSession(); err != nil {
		fmt.Printf("清除会话失败: %v\n", err)
		os.Exit(ExitCodeSessionError)
	}

	fmt.Println("会话已清除，下次启动将需要重新登录")
}
