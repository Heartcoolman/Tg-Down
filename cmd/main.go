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
	ModeDownloadHistory    = 1
	// ModeMonitorNewMessages is the mode for monitoring new messages.
	ModeMonitorNewMessages = 2
	// ModeDownloadAndMonitor is the mode for both downloading history and monitoring new messages.
	ModeDownloadAndMonitor = 3
)

func main() {
	// 检查命令行参数
	if len(os.Args) > 1 && os.Args[1] == "--clear-session" {
		clearSessionAndExit()
		return
	}

	// 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		fmt.Println("请确保已正确配置 config.yaml 或环境变量")
		fmt.Println("可以参考 config.yaml.example 和 .env.example 文件")
		os.Exit(1)
	}

	// 创建日志记录器
	log := logger.New(cfg.Log.Level)
	log.Info("Telegram群聊媒体下载器启动")

	// 创建上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 处理中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Info("收到中断信号，正在退出...")
		cancel()
	}()

	// 创建Telegram客户端
	client := telegram.New(cfg, log)

	// 连接到Telegram并运行主逻辑
	log.Info("正在连接到Telegram...")
	err = client.Client.Run(ctx, func(ctx context.Context) error {
		// 检查授权状态
		status, authErr := client.Client.Auth().Status(ctx)
		if authErr != nil {
			return fmt.Errorf("检查授权状态失败: %w", authErr)
		}

		if !status.Authorized {
			// 需要登录
			authErr := client.Authenticate(ctx)
			if authErr != nil {
				return fmt.Errorf("认证失败: %w", authErr)
			}
		}

		client.API = client.Client.API()
		log.Info("成功连接到Telegram")

		// 选择目标聊天
		var targetChatID int64
		if cfg.Chat.TargetID != 0 {
			targetChatID = cfg.Chat.TargetID
			log.Info("使用配置的聊天ID: %d", targetChatID)
		} else {
			targetChatID, err = selectChat(ctx, client, log)
			if err != nil {
				return fmt.Errorf("选择聊天失败: %w", err)
			}
		}

		// 询问用户操作模式
		mode := selectMode(log)

		switch mode {
		case ModeDownloadHistory:
			// 只下载历史媒体
			log.Info("开始下载历史媒体文件...")
			downloadErr := client.DownloadHistoryMedia(ctx, targetChatID)
			if downloadErr != nil {
				log.Error("下载历史媒体失败: %v", downloadErr)
			}

		case ModeMonitorNewMessages:
			// 只监控新消息
			log.Info("开始实时监控新消息...")
			client.SetupRealTimeMonitoring(targetChatID)

		case ModeDownloadAndMonitor:
			// 先下载历史，再监控新消息
			log.Info("开始下载历史媒体文件...")
			downloadErr := client.DownloadHistoryMedia(ctx, targetChatID)
			if downloadErr != nil {
				log.Error("下载历史媒体失败: %v", downloadErr)
			} else {
				log.Info("历史媒体下载完成，开始实时监控...")
				client.SetupRealTimeMonitoring(targetChatID)
			}
		}

		// 保持运行直到上下文取消
		<-ctx.Done()
		return nil
	})

	if err != nil {
		log.Error("运行失败: %v", err)
		os.Exit(1)
	}

	log.Info("程序退出")
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

	// 显示聊天列表
	fmt.Println("\n可用的聊天:")
	for i, chat := range chats {
		fmt.Printf("%d. %s (%s) - ID: %d\n", i+1, chat.Title, chat.Type, chat.ID)
	}

	// 让用户选择
	fmt.Print("\n请选择聊天 (输入序号): ")
	var choice int
	if _, scanErr := fmt.Scanln(&choice); scanErr != nil {
		log.Warn("读取输入失败: %v", scanErr)
		return 0, fmt.Errorf("输入无效")
	}

	if choice < 1 || choice > len(chats) {
		return 0, fmt.Errorf("选择无效")
	}

	selectedChat := chats[choice-1]
	log.Info("选择了聊天: %s (ID: %d)", selectedChat.Title, selectedChat.ID)
	return selectedChat.ID, nil
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
	if err != nil || mode < ModeDownloadHistory || mode > ModeDownloadAndMonitor {
		log.Warn("输入无效，使用默认模式 %d", ModeDownloadAndMonitor)
		return ModeDownloadAndMonitor
	}

	return mode
}

// clearSessionAndExit 清除会话文件并退出
func clearSessionAndExit() {
	fmt.Println("正在清除会话文件...")

	// 加载配置以获取会话目录
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}

	// 创建临时客户端以使用清除会话功能
	log := logger.New(config.DefaultLogLevel)
	client := telegram.New(cfg, log)

	if err := client.ClearSession(); err != nil {
		fmt.Printf("清除会话失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("会话文件已清除，下次启动将需要重新登录")
}
