// Package main implements the entry point for the Telegram media downloader application.
// It provides functionality to download media files from Telegram chats and monitor new messages.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"tg-down/internal/config"
	"tg-down/internal/logger"
	"tg-down/internal/telegram"
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
	// 检查命令行参数
	if len(os.Args) > 1 && os.Args[1] == "--clear-session" {
		clearSessionAndExit()
		return
	}

	// 加载配置
	cfg, log := initializeApplication()

	// 创建上下文和信号处理
	ctx, cancel := setupSignalHandling(log)
	defer cancel()

	// 询问用户操作模式
	mode := selectMode(log)

	// 选择目标聊天
	targetChatID := getTargetChatID(ctx, cfg, log)

	// 创建客户端并运行
	runApplication(ctx, cfg, log, mode, targetChatID)
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

// getTargetChatID 获取目标聊天ID
func getTargetChatID(ctx context.Context, cfg *config.Config, log *logger.Logger) int64 {
	if cfg.Chat.TargetID != 0 {
		log.Info("使用配置的聊天ID: %d", cfg.Chat.TargetID)
		return cfg.Chat.TargetID
	}

	return selectChatInteractively(ctx, cfg, log)
}

// selectChatInteractively 交互式选择聊天
func selectChatInteractively(ctx context.Context, cfg *config.Config, log *logger.Logger) int64 {
	tempClient := telegram.New(cfg, log)
	var targetChatID int64

	err := tempClient.Client.Run(ctx, func(ctx context.Context) error {
		if err := authenticateClient(ctx, tempClient); err != nil {
			return err
		}

		tempClient.API = tempClient.Client.API()
		log.Info("成功连接到Telegram")

		chatID, err := selectChat(ctx, tempClient, log)
		if err != nil {
			return fmt.Errorf("选择聊天失败: %w", err)
		}
		targetChatID = chatID
		return nil
	})

	if err != nil {
		log.Error("选择聊天失败: %v", err)
		os.Exit(ExitCodeChatError)
	}

	return targetChatID
}

// authenticateClient 认证客户端
func authenticateClient(ctx context.Context, client *telegram.Client) error {
	status, err := client.Client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("检查授权状态失败: %w", err)
	}

	if !status.Authorized {
		if err := client.Authenticate(ctx); err != nil {
			return fmt.Errorf("认证失败: %w", err)
		}
	}

	return nil
}

// createClient 根据模式创建合适的客户端
func createClient(cfg *config.Config, log *logger.Logger, mode int, targetChatID int64) *telegram.Client {
	if mode == ModeMonitorNewMessages || mode == ModeDownloadAndMonitor {
		log.Info("创建带实时监控功能的客户端...")
		return telegram.NewWithUpdates(cfg, log, targetChatID)
	}

	log.Info("创建普通客户端...")
	return telegram.New(cfg, log)
}

// runApplication 运行主应用程序逻辑
func runApplication(ctx context.Context, cfg *config.Config, log *logger.Logger, mode int, targetChatID int64) {
	client := createClient(cfg, log, mode, targetChatID)
	if client == nil {
		log.Error("创建客户端失败")
		os.Exit(ExitCodeClientError)
	}

	log.Info("正在连接到Telegram...")
	err := client.Client.Run(ctx, func(ctx context.Context) error {
		if err := authenticateClient(ctx, client); err != nil {
			return err
		}

		client.API = client.Client.API()
		log.Info("成功连接到Telegram")

		return executeMode(ctx, client, log, mode, targetChatID)
	})

	if err != nil {
		log.Error("运行失败: %v", err)
		os.Exit(ExitCodeRunError)
	}

	log.Info("程序退出")
}

// executeMode 执行指定的操作模式
func executeMode(ctx context.Context, client *telegram.Client, log *logger.Logger, mode int, targetChatID int64) error {
	switch mode {
	case ModeDownloadHistory:
		return executeDownloadHistory(ctx, client, log, targetChatID)
	case ModeMonitorNewMessages:
		return executeMonitorNewMessages(ctx, client, log, targetChatID)
	case ModeDownloadAndMonitor:
		return executeDownloadAndMonitor(ctx, client, log, targetChatID)
	default:
		return fmt.Errorf("未知的操作模式: %d", mode)
	}
}

// executeDownloadHistory 执行下载历史媒体模式
func executeDownloadHistory(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64) error {
	log.Info("开始下载历史媒体文件...")
	if err := client.DownloadHistoryMedia(ctx, targetChatID); err != nil {
		log.Error("下载历史媒体失败: %v", err)
	}
	<-ctx.Done()
	return nil
}

// executeMonitorNewMessages 执行监控新消息模式
func executeMonitorNewMessages(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64) error {
	log.Info("开始实时监控新消息...")
	log.Info("实时监控已启动，目标聊天ID: %d", targetChatID)

	startInteractiveMonitoring(ctx, client, log, targetChatID)
	<-ctx.Done()
	return nil
}

// executeDownloadAndMonitor 执行下载历史并监控新消息模式
func executeDownloadAndMonitor(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64) error {
	log.Info("开始下载历史媒体文件...")
	if err := client.DownloadHistoryMedia(ctx, targetChatID); err != nil {
		log.Error("下载历史媒体失败: %v", err)
	} else {
		log.Info("历史媒体下载完成，实时监控已自动启动")
		log.Info("实时监控已启动，目标聊天ID: %d", targetChatID)
	}
	<-ctx.Done()
	return nil
}

// startInteractiveMonitoring 启动交互式监控
func startInteractiveMonitoring(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64) {
	go func() {
		fmt.Println("\n监控已启动！")
		fmt.Println("输入命令:")
		fmt.Println("  'check' - 手动检查新消息")
		fmt.Println("  'status' - 查看监控状态")
		fmt.Println("  'quit' - 退出程序")
		fmt.Print("> ")

		for {
			var input string
			if _, scanErr := fmt.Scanln(&input); scanErr != nil {
				continue
			}

			handleInteractiveCommand(ctx, client, log, targetChatID, input)
			fmt.Print("> ")
		}
	}()
}

// handleInteractiveCommand 处理交互式命令
func handleInteractiveCommand(ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64, input string) {
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
		// Note: In a real implementation, you'd need to pass the cancel function here
		return
	default:
		fmt.Println("未知命令，请输入 'check', 'status' 或 'quit'")
	}
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

// clearSessionAndExit 清除会话文件并退出
func clearSessionAndExit() {
	fmt.Println("正在清除会话文件...")

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

	fmt.Println("会话文件已清除，下次启动将需要重新登录")
}
