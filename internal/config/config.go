package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Config 应用配置结构
type Config struct {
	API      APIConfig      `yaml:"api"`
	Download DownloadConfig `yaml:"download"`
	Chat     ChatConfig     `yaml:"chat"`
	Log      LogConfig      `yaml:"log"`
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

// LoadConfig 加载配置文件
func LoadConfig() (*Config, error) {
	// 尝试加载 .env 文件
	_ = godotenv.Load()

	config := &Config{}

	// 首先尝试从 YAML 文件加载
	if _, err := os.Stat("config.yaml"); err == nil {
		data, err := os.ReadFile("config.yaml")
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}

		if err := yaml.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %w", err)
		}
	}

	// 从环境变量覆盖配置
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

	if downloadPath := os.Getenv("DOWNLOAD_PATH"); downloadPath != "" {
		config.Download.Path = downloadPath
	}

	if maxConcurrent := os.Getenv("MAX_CONCURRENT_DOWNLOADS"); maxConcurrent != "" {
		if max, err := strconv.Atoi(maxConcurrent); err == nil {
			config.Download.MaxConcurrent = max
		}
	}

	if batchSize := os.Getenv("BATCH_SIZE"); batchSize != "" {
		if batch, err := strconv.Atoi(batchSize); err == nil {
			config.Download.BatchSize = batch
		}
	}

	if targetChatID := os.Getenv("TARGET_CHAT_ID"); targetChatID != "" {
		if chatID, err := strconv.ParseInt(targetChatID, 10, 64); err == nil {
			config.Chat.TargetID = chatID
		}
	}

	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		config.Log.Level = logLevel
	}

	// 设置默认值
	if config.Download.Path == "" {
		config.Download.Path = "./downloads"
	}
	if config.Download.MaxConcurrent == 0 {
		config.Download.MaxConcurrent = 5
	}
	if config.Download.BatchSize == 0 {
		config.Download.BatchSize = 100
	}
	if config.Log.Level == "" {
		config.Log.Level = "info"
	}

	// 验证必要配置
	if config.API.ID == 0 || config.API.Hash == "" || config.API.Phone == "" {
		return nil, fmt.Errorf("缺少必要的API配置: API_ID, API_HASH, PHONE")
	}

	return config, nil
}

// SaveConfig 保存配置到文件
func (c *Config) SaveConfig(filename string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}

	return nil
}
