// Package main implements a connection test utility for Telegram API.
// It verifies network connectivity and API accessibility before running the main application.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/logger"

	"github.com/gotd/td/telegram"
)

const (
	// TestTimeout is the timeout duration for connection tests
	TestTimeout = 10 * time.Second
	// TelegramAPIHost is the main Telegram API host
	TelegramAPIHost = "api.telegram.org"
	// TelegramAPIPort is the standard Telegram API port
	TelegramAPIPort = "443"
	// SeparatorLength is the length of separator line
	SeparatorLength = 50
	// BadRequestCode is the HTTP status code for bad request
	BadRequestCode = 400
)

func main() {
	fmt.Println("🔍 Telegram 连接测试工具")
	fmt.Println(strings.Repeat("=", SeparatorLength))

	// 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("❌ 加载配置失败: %v\n", err)
		fmt.Println("请确保已正确配置 config.yaml 或环境变量")
		os.Exit(1)
	}

	// 创建日志记录器
	log := logger.New(cfg.Log.Level)

	// 执行连接测试
	success := runConnectionTests(cfg, log)

	if success {
		fmt.Println("\n✅ 所有连接测试通过！可以安全运行主程序。")
		os.Exit(0)
	}
	fmt.Println("\n❌ 连接测试失败！请检查网络连接和防火墙设置。")
	os.Exit(1)
}

// runConnectionTests 执行所有连接测试
func runConnectionTests(cfg *config.Config, log *logger.Logger) bool {
	tests := []struct {
		name string
		test func() error
	}{
		{"DNS 解析测试", testDNSResolution},
		{"TCP 连接测试", testTCPConnection},
		{"HTTP/HTTPS 连接测试", testHTTPConnection},
		{"Telegram API 基础连接测试", func() error { return testTelegramAPIConnection(cfg, log) }},
	}

	allPassed := true
	for _, test := range tests {
		fmt.Printf("🔄 执行 %s...", test.name)

		start := time.Now()
		err := test.test()
		duration := time.Since(start)

		if err != nil {
			fmt.Printf(" ❌ 失败 (%.2fs)\n", duration.Seconds())
			fmt.Printf("   错误: %v\n", err)
			allPassed = false
		} else {
			fmt.Printf(" ✅ 通过 (%.2fs)\n", duration.Seconds())
		}
	}

	return allPassed
}

// testDNSResolution 测试 DNS 解析
func testDNSResolution() error {
	ctx, cancel := context.WithTimeout(context.Background(), TestTimeout)
	defer cancel()

	resolver := &net.Resolver{}
	_, err := resolver.LookupHost(ctx, TelegramAPIHost)
	if err != nil {
		return fmt.Errorf("无法解析 %s: %w", TelegramAPIHost, err)
	}

	return nil
}

// testTCPConnection 测试 TCP 连接
func testTCPConnection() error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(TelegramAPIHost, TelegramAPIPort), TestTimeout)
	if err != nil {
		return fmt.Errorf("无法建立 TCP 连接到 %s:%s: %w", TelegramAPIHost, TelegramAPIPort, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			fmt.Printf("failed to close connection: %v\n", err)
		}
	}()

	return nil
}

// testHTTPConnection 测试 HTTP/HTTPS 连接
func testHTTPConnection() error {
	ctx, cancel := context.WithTimeout(context.Background(), TestTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: TestTimeout,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+TelegramAPIHost, nil)
	if err != nil {
		return fmt.Errorf("无法创建 HTTP 请求: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("无法建立 HTTPS 连接: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("failed to close response body: %v\n", err)
		}
	}()

	if resp.StatusCode >= BadRequestCode {
		return fmt.Errorf("HTTP 响应错误: %d %s", resp.StatusCode, resp.Status)
	}

	return nil
}

// testTelegramAPIConnection 测试 Telegram API 连接
func testTelegramAPIConnection(cfg *config.Config, _ *logger.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), TestTimeout)
	defer cancel()

	// 创建 Telegram 客户端选项
	options := telegram.Options{}

	// 创建客户端
	client := telegram.NewClient(cfg.API.ID, cfg.API.Hash, options)

	// 尝试连接并获取配置
	err := client.Run(ctx, func(ctx context.Context) error {
		api := client.API()

		// 尝试获取配置信息（这是一个轻量级的 API 调用）
		_, configErr := api.HelpGetConfig(ctx)
		if configErr != nil {
			return fmt.Errorf("无法获取 telegram 配置: %w", configErr)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("telegram API 连接失败: %w", err)
	}

	return nil
}
