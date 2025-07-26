package main

import (
	"fmt"
	"os"

	"tg-down/internal/config"
	"tg-down/internal/logger"
)

func main() {
	fmt.Println("Telegram媒体下载器 - 配置测试")
	fmt.Println("================================")

	// 测试配置加载
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("❌ 配置加载失败: %v\n", err)
		fmt.Println("\n请检查以下配置:")
		fmt.Println("1. 复制 config.yaml.example 到 config.yaml")
		fmt.Println("2. 或者复制 .env.example 到 .env")
		fmt.Println("3. 设置正确的 API_ID, API_HASH 和 PHONE")
		os.Exit(1)
	}

	fmt.Println("✅ 配置加载成功")

	// 验证必要配置
	if cfg.API.ID == 0 {
		fmt.Println("❌ API_ID 未设置")
	} else {
		fmt.Printf("✅ API_ID: %d\n", cfg.API.ID)
	}

	if cfg.API.Hash == "" {
		fmt.Println("❌ API_HASH 未设置")
	} else {
		fmt.Printf("✅ API_HASH: %s...\n", cfg.API.Hash[:8])
	}

	if cfg.API.Phone == "" {
		fmt.Println("❌ PHONE 未设置")
	} else {
		fmt.Printf("✅ PHONE: %s\n", cfg.API.Phone)
	}

	// 测试日志
	log := logger.New(cfg.Log.Level)
	fmt.Printf("✅ 日志级别: %s\n", cfg.Log.Level)

	// 测试下载目录
	if err := os.MkdirAll(cfg.Download.Path, 0755); err != nil {
		fmt.Printf("❌ 无法创建下载目录 %s: %v\n", cfg.Download.Path, err)
	} else {
		fmt.Printf("✅ 下载目录: %s\n", cfg.Download.Path)
	}

	fmt.Printf("✅ 最大并发下载数: %d\n", cfg.Download.MaxConcurrent)
	fmt.Printf("✅ 批量处理大小: %d\n", cfg.Download.BatchSize)

	if cfg.Chat.TargetID != 0 {
		fmt.Printf("✅ 目标聊天ID: %d\n", cfg.Chat.TargetID)
	} else {
		fmt.Println("ℹ️  目标聊天ID未设置，运行时将提示选择")
	}

	fmt.Println("\n🎉 配置验证完成！")
	fmt.Println("现在可以运行主程序: go run cmd/main.go")

	log.Info("配置测试完成")
}
