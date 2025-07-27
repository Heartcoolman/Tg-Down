// Package config provides configuration management for Tg-Down application.
// It supports loading configuration from YAML files and environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// 默认配置常量
const (
	DefaultDownloadPath = "./downloads"
	DefaultLogLevel     = "info"
	DefaultSessionDir   = "./sessions"
	// FilePermission is the permission mode for creating config files
	FilePermission = 0600
)

// Config 应用配置结构
type Config struct {
	API      APIConfig      `yaml:"api"`
	Download DownloadConfig `yaml:"download"`
	Chat     ChatConfig     `yaml:"chat"`
	Log      LogConfig      `yaml:"log"`
	Session  SessionConfig  `yaml:"session"`
}

// APIConfig Telegram API配置
type APIConfig struct {
	ID    int    `yaml:"id"`
	Hash  string `yaml:"hash"`
	Phone string `yaml:"phone"`
}

// DownloadConfig 下载配置
type DownloadConfig struct {
	Path          string `yaml:"path"`
	MaxConcurrent int    `yaml:"max_concurrent"`
	BatchSize     int    `yaml:"batch_size"`
}

// ChatConfig 聊天配置
type ChatConfig struct {
	TargetID int64 `yaml:"target_id"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level string `yaml:"level"`
}

// SessionConfig 会话配置
type SessionConfig struct {
	Dir string `yaml:"dir"`
}

// LoadConfig 加载配置文件
func LoadConfig() (*Config, error) {
	// 尝试加载 .env 文件
	_ = godotenv.Load()

	config := &Config{}

	// 从 YAML 文件加载配置
	if err := loadFromYAML(config); err != nil {
		return nil, err
	}

	// 从环境变量覆盖配置
	loadFromEnv(config)

	// 设置默认值
	setDefaults(config)

	// 验证必要配置
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	return config, nil
}

// loadFromYAML 从YAML文件加载配置
func loadFromYAML(config *Config) error {
	if _, err := os.Stat("config.yaml"); err != nil {
		return nil // 文件不存在，跳过
	}

	data, err := os.ReadFile("config.yaml")
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(data, config); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	return nil
}

// loadFromEnv 从环境变量加载配置
func loadFromEnv(config *Config) {
	loadAPIConfig(config)
	loadDownloadConfig(config)
	loadChatConfig(config)
	loadLogConfig(config)
	loadSessionConfig(config)
}

// loadAPIConfig 加载API配置
func loadAPIConfig(config *Config) {
	if apiID := os.Getenv("API_ID"); apiID != "" {
		if id, err := strconv.Atoi(apiID); err == nil {
			config.API.ID = id
		}
	}

	if apiHash := os.Getenv("API_HASH"); apiHash != "" {
		config.API.Hash = apiHash
	}

	if phone := os.Getenv("PHONE"); phone != "" {
		config.API.Phone = phone
	}
}

// loadDownloadConfig 加载下载配置
func loadDownloadConfig(config *Config) {
	if downloadPath := os.Getenv("DOWNLOAD_PATH"); downloadPath != "" {
		config.Download.Path = downloadPath
	}

	if maxConcurrent := os.Getenv("MAX_CONCURRENT_DOWNLOADS"); maxConcurrent != "" {
		if maxValue, err := strconv.Atoi(maxConcurrent); err == nil {
			config.Download.MaxConcurrent = maxValue
		}
	}

	if batchSize := os.Getenv("BATCH_SIZE"); batchSize != "" {
		if batch, err := strconv.Atoi(batchSize); err == nil {
			config.Download.BatchSize = batch
		}
	}
}

// loadChatConfig 加载聊天配置
func loadChatConfig(config *Config) {
	if targetChatID := os.Getenv("TARGET_CHAT_ID"); targetChatID != "" {
		if chatID, err := strconv.ParseInt(targetChatID, 10, 64); err == nil {
			config.Chat.TargetID = chatID
		}
	}
}

// loadLogConfig 加载日志配置
func loadLogConfig(config *Config) {
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		config.Log.Level = logLevel
	}
}

// loadSessionConfig 加载会话配置
func loadSessionConfig(config *Config) {
	if sessionDir := os.Getenv("SESSION_DIR"); sessionDir != "" {
		config.Session.Dir = sessionDir
	}
}

// setDefaults 设置默认值
func setDefaults(config *Config) {
	if config.Download.Path == "" {
		config.Download.Path = DefaultDownloadPath
	}
	if config.Download.MaxConcurrent == 0 {
		config.Download.MaxConcurrent = 5
	}
	if config.Download.BatchSize == 0 {
		config.Download.BatchSize = 100
	}
	if config.Log.Level == "" {
		config.Log.Level = DefaultLogLevel
	}
	if config.Session.Dir == "" {
		config.Session.Dir = DefaultSessionDir
	}
}

// validateConfig 验证配置
func validateConfig(config *Config) error {
	if config.API.ID == 0 || config.API.Hash == "" || config.API.Phone == "" {
		return fmt.Errorf("缺少必要的API配置: API_ID, API_HASH, PHONE")
	}
	return nil
}

// SaveConfig 保存配置到文件
func (c *Config) SaveConfig(filename string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(filename, data, FilePermission); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}

	return nil
}
