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

	// 默认下载配置
	DefaultMaxConcurrent = 5
	DefaultBatchSize     = 100
	// DefaultPartitionSize 是历史下载的在途媒体上限（扫描最多领先下载的数量）
	DefaultPartitionSize = 100

	// 默认重试配置
	DefaultMaxRetries = 3
	DefaultBaseDelay  = 1  // 1秒
	DefaultMaxDelay   = 30 // 30秒

	// 默认队列配置
	DefaultMaxConcurrentTasks = 1

	// 默认存储配置
	DefaultStorePath = "./tg-down.db"

	// 进制转换基数
	DecimalBase  = 10
	FloatBitSize = 64
)

// Config 应用配置结构
type Config struct {
	API      APIConfig      `yaml:"api"`
	Download DownloadConfig `yaml:"download"`
	Chat     ChatConfig     `yaml:"chat"`
	Log      LogConfig      `yaml:"log"`
	Session  SessionConfig  `yaml:"session"`
	Retry    RetryConfig    `yaml:"retry"`
	Queue    QueueConfig    `yaml:"queue"`
	Store    StoreConfig    `yaml:"store"`
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
	MaxConcurrent int    `yaml:"max_concurrent"` // 同时下载的文件数
	BatchSize     int    `yaml:"batch_size"`     // 每批拉取的历史消息数
	PartitionSize int    `yaml:"partition_size"` // 历史下载在途媒体上限（扫描最多领先下载的数量）
	// DisableClassifyByType 为 true 时关闭按媒体类型归档（默认归档开启）
	DisableClassifyByType bool `yaml:"disable_classify_by_type"`
}

// RetryConfig 重试配置
type RetryConfig struct {
	MaxRetries int `yaml:"max_retries"` // 最大重试次数
	BaseDelay  int `yaml:"base_delay"`  // 基础延迟 (秒)
	MaxDelay   int `yaml:"max_delay"`   // 最大延迟 (秒)
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
	Dir string `yaml:"dir"` // TDLib 数据库/会话根目录（实际数据库位于 <dir>/tdlib）
}

// QueueConfig 任务队列配置
type QueueConfig struct {
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks"` // 同时运行的历史下载任务数（监控任务不占用此配额，独立运行）
}

// StoreConfig 持久化存储配置
type StoreConfig struct {
	Path string `yaml:"path"` // SQLite 数据库文件路径
}

// LoadConfig 加载配置文件（要求 API 凭据齐全，用于 CLI 模式）
func LoadConfig() (*Config, error) {
	return load(true)
}

// LoadConfigForWeb 加载配置但不强制 API 凭据；Web 模式允许在页面内补填凭据后再连接
func LoadConfigForWeb() (*Config, error) {
	return load(false)
}

func load(requireAPI bool) (*Config, error) {
	// 尝试加载 .env 文件
	_ = godotenv.Load()

	config := &Config{}

	// 从 YAML 文件加载配置
	if err := loadFromYAML(config); err != nil {
		return nil, err
	}

	// 从环境变量覆盖配置
	loadFromEnv(config)
	warnRemovedEnv()

	// 设置默认值
	setDefaults(config)

	// 验证必要配置
	if requireAPI {
		if err := validateConfig(config); err != nil {
			return nil, err
		}
	}

	return config, nil
}

// HasAPICredentials 判断 API 凭据（id/hash/phone）是否齐全
func (c *Config) HasAPICredentials() bool {
	return c.API.ID != 0 && c.API.Hash != "" && c.API.Phone != ""
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

	warnRemovedYAMLKeys(data)
	return nil
}

// warnRemovedYAMLKeys 检测 v2.0 移除的配置键并警告（yaml.v3 对未知键静默丢弃，
// 不显式检测用户无从得知配置已失效）。配置加载先于 logger 初始化，直接写 stderr。
func warnRemovedYAMLKeys(data []byte) {
	var raw map[string]any
	if yaml.Unmarshal(data, &raw) != nil {
		return
	}
	if _, ok := raw["rate_limit"]; ok {
		warnRemoved("配置项 rate_limit.* 已在 v2.0 移除（TDLib 内部处理限流），请从 config.yaml 删除")
	}
	if dl, ok := raw["download"].(map[string]any); ok {
		if _, ok := dl["chunk_size"]; ok {
			warnRemoved("配置项 download.chunk_size 已在 v2.0 移除（TDLib 自管分片），请从 config.yaml 删除")
		}
		if _, ok := dl["max_workers"]; ok {
			warnRemoved("配置项 download.max_workers 已在 v2.0 移除（TDLib 自管单文件并行度），请从 config.yaml 删除")
		}
	}
}

// warnRemovedEnv 检测 v2.0 移除的环境变量并警告
func warnRemovedEnv() {
	for _, name := range []string{"CHUNK_SIZE", "MAX_WORKERS", "REQUESTS_PER_SECOND", "BURST_SIZE"} {
		if os.Getenv(name) != "" {
			warnRemoved(fmt.Sprintf("环境变量 %s 已在 v2.0 移除且不再生效", name))
		}
	}
}

func warnRemoved(msg string) {
	fmt.Fprintf(os.Stderr, "[配置警告] %s\n", msg)
}

// loadFromEnv 从环境变量加载配置
func loadFromEnv(config *Config) {
	loadAPIConfig(config)
	loadDownloadConfig(config)
	loadChatConfig(config)
	loadLogConfig(config)
	loadSessionConfig(config)
	loadRetryConfig(config)
	loadQueueConfig(config)
	loadStoreConfig(config)
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

	if partitionSize := os.Getenv("PARTITION_SIZE"); partitionSize != "" {
		if partition, err := strconv.Atoi(partitionSize); err == nil {
			config.Download.PartitionSize = partition
		}
	}
}

// loadChatConfig 加载聊天配置
func loadChatConfig(config *Config) {
	if targetChatID := os.Getenv("TARGET_CHAT_ID"); targetChatID != "" {
		if chatID, err := strconv.ParseInt(targetChatID, DecimalBase, FloatBitSize); err == nil {
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

// loadRetryConfig 加载重试配置
func loadRetryConfig(config *Config) {
	if maxRetries := os.Getenv("MAX_RETRIES"); maxRetries != "" {
		if retries, err := strconv.Atoi(maxRetries); err == nil {
			config.Retry.MaxRetries = retries
		}
	}

	if baseDelay := os.Getenv("BASE_DELAY"); baseDelay != "" {
		if delay, err := strconv.Atoi(baseDelay); err == nil {
			config.Retry.BaseDelay = delay
		}
	}

	if maxDelay := os.Getenv("MAX_DELAY"); maxDelay != "" {
		if delay, err := strconv.Atoi(maxDelay); err == nil {
			config.Retry.MaxDelay = delay
		}
	}
}

// loadQueueConfig 加载队列配置
func loadQueueConfig(config *Config) {
	if maxConcurrentTasks := os.Getenv("MAX_CONCURRENT_TASKS"); maxConcurrentTasks != "" {
		if tasks, err := strconv.Atoi(maxConcurrentTasks); err == nil {
			config.Queue.MaxConcurrentTasks = tasks
		}
	}
}

// loadStoreConfig 加载存储配置
func loadStoreConfig(config *Config) {
	if storePath := os.Getenv("STORE_PATH"); storePath != "" {
		config.Store.Path = storePath
	}
}

// setDefaults 设置默认值
func setDefaults(config *Config) {
	if config.Download.Path == "" {
		config.Download.Path = DefaultDownloadPath
	}
	if config.Download.MaxConcurrent <= 0 {
		config.Download.MaxConcurrent = DefaultMaxConcurrent
	}
	// 以下数值项统一用 <= 0 守卫：负值与 0 一样回退默认值，避免负的 BatchSize/延迟/重试次数
	// 通过校验后进入下载/退避热路径（如负 BaseDelay 使退避为负、time.After 立即触发导致零延迟热重试）。
	if config.Download.BatchSize <= 0 {
		config.Download.BatchSize = DefaultBatchSize
	}
	if config.Download.PartitionSize <= 0 {
		config.Download.PartitionSize = DefaultPartitionSize
	}

	if config.Retry.MaxRetries <= 0 {
		config.Retry.MaxRetries = DefaultMaxRetries
	}
	if config.Retry.BaseDelay <= 0 {
		config.Retry.BaseDelay = DefaultBaseDelay
	}
	if config.Retry.MaxDelay <= 0 {
		config.Retry.MaxDelay = DefaultMaxDelay
	}

	if config.Log.Level == "" {
		config.Log.Level = DefaultLogLevel
	}
	if config.Session.Dir == "" {
		config.Session.Dir = DefaultSessionDir
	}

	if config.Queue.MaxConcurrentTasks <= 0 {
		config.Queue.MaxConcurrentTasks = DefaultMaxConcurrentTasks
	}
	if config.Store.Path == "" {
		config.Store.Path = DefaultStorePath
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
