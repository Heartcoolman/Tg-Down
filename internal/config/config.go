// Package config provides configuration management for Tg-Down application.
// It supports loading configuration from YAML files and environment variables.
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"

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
	// DefaultAutoRetry 是 history 任务失败后的自动重试上限（0 = 关闭）
	DefaultAutoRetry = 2

	// 默认存储配置
	DefaultStorePath = "./tg-down.db"

	// 进制转换基数
	DecimalBase  = 10
	FloatBitSize = 64
	// MaxTelegramAPIID 是 TDLib int32 字段可接受的最大 API ID。
	MaxTelegramAPIID = 1<<31 - 1
	// apiHashLength 是 Telegram API Hash 的十六进制字符数。
	apiHashLength = 32

	// osWindows 用于 runtime.GOOS 比较：Windows 无 POSIX 权限位，权限收紧逻辑需跳过。
	osWindows = "windows"
)

// Config 应用配置结构
type Config struct {
	API      APIConfig      `yaml:"api"`
	Telegram TelegramConfig `yaml:"telegram"`
	Download DownloadConfig `yaml:"download"`
	Chat     ChatConfig     `yaml:"chat"`
	Log      LogConfig      `yaml:"log"`
	Session  SessionConfig  `yaml:"session"`
	Retry    RetryConfig    `yaml:"retry"`
	Queue    QueueConfig    `yaml:"queue"`
	Store    StoreConfig    `yaml:"store"`
	Notify   NotifyConfig   `yaml:"notify"`
}

// NotifyConfig 任务完成通知配置
type NotifyConfig struct {
	// TelegramSelf 为 true 时任务终结（完成/最终失败）向自己的 Saved Messages 发消息
	TelegramSelf bool `yaml:"telegram_self"`
	// WebhookURL 非空时任务终结向该地址 POST JSON
	WebhookURL string `yaml:"webhook_url"`
}

// APIConfig Telegram API配置
type APIConfig struct {
	ID    int    `yaml:"id"`
	Hash  string `yaml:"hash"`
	Phone string `yaml:"phone"`
}

// TelegramConfig Telegram 连接配置
type TelegramConfig struct {
	// Proxy 是 TDLib 连接 Telegram 使用的代理。TDLib 不读 HTTP_PROXY/HTTPS_PROXY
	// 等环境变量，服务器所在网络无法直连 Telegram 时必须在此显式配置（issue #49）。
	// 支持格式：
	//   socks5://[user:pass@]host:port
	//   http://[user:pass@]host:port   （HTTP CONNECT，https:// 写法也按此处理）
	//   mtproto://secret@host:port
	// 留空时回退环境变量 TG_PROXY > ALL_PROXY > HTTPS_PROXY > HTTP_PROXY；
	// 设为 direct / off / none 可在存在上述环境变量时强制直连。
	Proxy string `yaml:"proxy"`
}

// DownloadConfig 下载配置
type DownloadConfig struct {
	Path          string `yaml:"path"`
	MaxConcurrent int    `yaml:"max_concurrent"` // 同时下载的文件数
	BatchSize     int    `yaml:"batch_size"`     // 每批拉取的历史消息数
	PartitionSize int    `yaml:"partition_size"` // 历史下载在途媒体上限（扫描最多领先下载的数量）
	// SaveMetadata 为 true 时在每个下载文件旁写 <文件>.json 元数据（caption/发送者/日期等）
	SaveMetadata bool `yaml:"save_metadata"`
	// DisableClassifyByType 为 true 时关闭按媒体类型归档（默认归档开启）
	DisableClassifyByType bool `yaml:"disable_classify_by_type"`
	// PathTemplate 是落盘路径模板，空 = downloader.DefaultPathTemplate（即 v2.x 的既有布局）。
	// 可用占位符：{chat_id} {chat_title} {type} {album} {date} {msg_id} {sender} {name} {ext}，
	// 必须包含 {chat_id}，并至少包含 {name} 或 {msg_id}（否则跨聊天或同聊天文件会互相覆盖）。
	PathTemplate string `yaml:"path_template"`
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
	// File 非空时日志同时写入该文件（按大小轮转，见 logger 包默认参数）。
	// 留空仅输出到控制台；桌面客户端默认填入应用数据目录下的 logs/tg-down.log
	File string `yaml:"file"`
}

// SessionConfig 会话配置
type SessionConfig struct {
	Dir string `yaml:"dir"` // TDLib 数据库/会话根目录（实际数据库位于 <dir>/tdlib）
}

// QueueConfig 任务队列配置
type QueueConfig struct {
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks"` // 同时运行的历史下载任务数（监控任务不占用此配额，独立运行）
	// AutoRetry 是 history 任务失败后的自动重试上限；nil（未配置）取默认值，显式 0 关闭
	AutoRetry *int `yaml:"auto_retry"`
}

// AutoRetryCount 返回生效的自动重试上限（未配置时为 DefaultAutoRetry）
func (q QueueConfig) AutoRetryCount() int {
	if q.AutoRetry == nil {
		return DefaultAutoRetry
	}
	if *q.AutoRetry < 0 {
		return 0
	}
	return *q.AutoRetry
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
	if err := loadDotEnv(); err != nil {
		return nil, err
	}

	config := &Config{}

	// 从 YAML 文件加载配置
	if err := loadFromYAML(config); err != nil {
		return nil, err
	}

	// 从环境变量覆盖配置
	if err := loadFromEnv(config); err != nil {
		return nil, err
	}
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

func loadDotEnv() error {
	if runtime.GOOS != osWindows {
		info, err := os.Stat(".env")
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(".env", FilePermission); err != nil {
				return fmt.Errorf("收紧 .env 文件权限失败: %w", err)
			}
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("检查 .env 文件失败: %w", err)
		}
	}
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("加载 .env 文件失败: %w", err)
	}
	return nil
}

// LoadSessionDir 只读取清理本地 TDLib 会话所需的目录。
// API 凭据或其他数值配置损坏时，--clear-session 仍必须可用。
func LoadSessionDir() (string, error) {
	if err := loadDotEnv(); err != nil {
		return "", err
	}

	var subset struct {
		Session SessionConfig `yaml:"session"`
	}
	data, err := os.ReadFile("config.yaml")
	if err == nil {
		if err := yaml.Unmarshal(data, &subset); err != nil {
			return "", fmt.Errorf("解析配置文件失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("读取配置文件失败: %w", err)
	}

	dir := subset.Session.Dir
	if envDir := os.Getenv("SESSION_DIR"); envDir != "" {
		dir = envDir
	}
	if dir == "" {
		dir = DefaultSessionDir
	}
	return dir, nil
}

// HasAPICredentials 判断 API 凭据（id/hash/phone）是否齐全
func (c *Config) HasAPICredentials() bool {
	return IsValidAPIID(int64(c.API.ID)) && IsValidAPIHash(c.API.Hash) && IsValidPhone(c.API.Phone)
}

// IsValidAPIID 报告 API ID 是否能安全传给 TDLib 的 int32 字段。
func IsValidAPIID(apiID int64) bool {
	return apiID > 0 && apiID <= MaxTelegramAPIID
}

// IsValidAPIHash 报告 API Hash 是否为 Telegram 要求的 32 位十六进制字符串。
func IsValidAPIHash(hash string) bool {
	if len(hash) != apiHashLength {
		return false
	}
	for i := range len(hash) {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// IsValidPhone 报告手机号是否符合 E.164 的基本格式：+ 后跟 7 到 15 位数字，首位不能为 0。
func IsValidPhone(phone string) bool {
	if len(phone) < 8 || len(phone) > 16 || phone[0] != '+' || phone[1] < '1' || phone[1] > '9' {
		return false
	}
	for i := 2; i < len(phone); i++ {
		if phone[i] < '0' || phone[i] > '9' {
			return false
		}
	}
	return true
}

// loadFromYAML 从YAML文件加载配置
func loadFromYAML(config *Config) error {
	f, err := os.Open("config.yaml")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("检查配置文件失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("配置文件 config.yaml 不是普通文件")
	}
	if runtime.GOOS != osWindows && info.Mode().Perm()&0o077 != 0 {
		if err := f.Chmod(FilePermission); err != nil {
			return fmt.Errorf("收紧配置文件权限失败: %w", err)
		}
	}

	data, err := io.ReadAll(f)
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
func loadFromEnv(config *Config) error {
	if err := loadAPIConfig(config); err != nil {
		return err
	}
	if err := loadDownloadConfig(config); err != nil {
		return err
	}
	if err := loadChatConfig(config); err != nil {
		return err
	}
	loadLogConfig(config)
	loadSessionConfig(config)
	if err := loadRetryConfig(config); err != nil {
		return err
	}
	if err := loadQueueConfig(config); err != nil {
		return err
	}
	loadStoreConfig(config)
	return loadNotifyConfig(config)
}

// loadNotifyConfig 加载通知配置
func loadNotifyConfig(config *Config) error {
	if v := os.Getenv("NOTIFY_TELEGRAM_SELF"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return invalidEnv("NOTIFY_TELEGRAM_SELF", v, err)
		}
		config.Notify.TelegramSelf = enabled
	}
	if v := os.Getenv("NOTIFY_WEBHOOK_URL"); v != "" {
		config.Notify.WebhookURL = v
	}
	return nil
}

// loadAPIConfig 加载API配置
func loadAPIConfig(config *Config) error {
	if apiID := os.Getenv("API_ID"); apiID != "" {
		id, err := strconv.Atoi(apiID)
		if err != nil {
			return invalidEnv("API_ID", apiID, err)
		}
		config.API.ID = id
	}

	if apiHash := os.Getenv("API_HASH"); apiHash != "" {
		config.API.Hash = apiHash
	}

	if phone := os.Getenv("PHONE"); phone != "" {
		config.API.Phone = phone
	}
	return nil
}

// loadDownloadConfig 加载下载配置
func loadDownloadConfig(config *Config) error {
	if downloadPath := os.Getenv("DOWNLOAD_PATH"); downloadPath != "" {
		config.Download.Path = downloadPath
	}

	if maxConcurrent := os.Getenv("MAX_CONCURRENT_DOWNLOADS"); maxConcurrent != "" {
		maxValue, err := strconv.Atoi(maxConcurrent)
		if err != nil {
			return invalidEnv("MAX_CONCURRENT_DOWNLOADS", maxConcurrent, err)
		}
		config.Download.MaxConcurrent = maxValue
	}

	if batchSize := os.Getenv("BATCH_SIZE"); batchSize != "" {
		batch, err := strconv.Atoi(batchSize)
		if err != nil {
			return invalidEnv("BATCH_SIZE", batchSize, err)
		}
		config.Download.BatchSize = batch
	}

	if partitionSize := os.Getenv("PARTITION_SIZE"); partitionSize != "" {
		partition, err := strconv.Atoi(partitionSize)
		if err != nil {
			return invalidEnv("PARTITION_SIZE", partitionSize, err)
		}
		config.Download.PartitionSize = partition
	}

	if saveMetadata := os.Getenv("SAVE_METADATA"); saveMetadata != "" {
		enabled, err := strconv.ParseBool(saveMetadata)
		if err != nil {
			return invalidEnv("SAVE_METADATA", saveMetadata, err)
		}
		config.Download.SaveMetadata = enabled
	}
	return nil
}

// loadChatConfig 加载聊天配置
func loadChatConfig(config *Config) error {
	if targetChatID := os.Getenv("TARGET_CHAT_ID"); targetChatID != "" {
		chatID, err := strconv.ParseInt(targetChatID, DecimalBase, FloatBitSize)
		if err != nil {
			return invalidEnv("TARGET_CHAT_ID", targetChatID, err)
		}
		config.Chat.TargetID = chatID
	}
	return nil
}

// loadLogConfig 加载日志配置
func loadLogConfig(config *Config) {
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		config.Log.Level = logLevel
	}
	if logFile := os.Getenv("LOG_FILE"); logFile != "" {
		config.Log.File = logFile
	}
}

// loadSessionConfig 加载会话配置
func loadSessionConfig(config *Config) {
	if sessionDir := os.Getenv("SESSION_DIR"); sessionDir != "" {
		config.Session.Dir = sessionDir
	}
}

// loadRetryConfig 加载重试配置
func loadRetryConfig(config *Config) error {
	if maxRetries := os.Getenv("MAX_RETRIES"); maxRetries != "" {
		retries, err := strconv.Atoi(maxRetries)
		if err != nil {
			return invalidEnv("MAX_RETRIES", maxRetries, err)
		}
		config.Retry.MaxRetries = retries
	}

	if baseDelay := os.Getenv("BASE_DELAY"); baseDelay != "" {
		delay, err := strconv.Atoi(baseDelay)
		if err != nil {
			return invalidEnv("BASE_DELAY", baseDelay, err)
		}
		config.Retry.BaseDelay = delay
	}

	if maxDelay := os.Getenv("MAX_DELAY"); maxDelay != "" {
		delay, err := strconv.Atoi(maxDelay)
		if err != nil {
			return invalidEnv("MAX_DELAY", maxDelay, err)
		}
		config.Retry.MaxDelay = delay
	}
	return nil
}

// loadQueueConfig 加载队列配置
func loadQueueConfig(config *Config) error {
	if maxConcurrentTasks := os.Getenv("MAX_CONCURRENT_TASKS"); maxConcurrentTasks != "" {
		tasks, err := strconv.Atoi(maxConcurrentTasks)
		if err != nil {
			return invalidEnv("MAX_CONCURRENT_TASKS", maxConcurrentTasks, err)
		}
		config.Queue.MaxConcurrentTasks = tasks
	}
	if autoRetry := os.Getenv("AUTO_RETRY"); autoRetry != "" {
		n, err := strconv.Atoi(autoRetry)
		if err != nil {
			return invalidEnv("AUTO_RETRY", autoRetry, err)
		}
		config.Queue.AutoRetry = &n
	}
	return nil
}

func invalidEnv(name, value string, err error) error {
	return fmt.Errorf("环境变量 %s 的值 %q 无效: %w", name, value, err)
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
	if !IsValidAPIID(int64(config.API.ID)) {
		return fmt.Errorf("API_ID 必须是 1 到 %d 之间的整数", MaxTelegramAPIID)
	}
	if !IsValidAPIHash(config.API.Hash) {
		return fmt.Errorf("API_HASH 必须为 32 位十六进制字符串")
	}
	if !IsValidPhone(config.API.Phone) {
		return fmt.Errorf("PHONE 必须为国际格式（+ 后跟 7 到 15 位数字）")
	}
	return nil
}

// saveConfigMu 串行化并发 SaveConfig：unix 上 rename 原子替换让并发写"碰巧"安全
// （last-write-wins），Windows 的 MoveFileEx 替换被占用目标会因共享冲突瞬时失败。
// 单进程内加互斥锁即获得全平台确定性行为。
var saveConfigMu sync.Mutex

// SaveConfig 保存配置到文件。设置 TG_DOWN_NO_CONFIG_WRITE 环境变量时跳过写入
// （容器等纯环境变量部署场景，配置由 env 提供，不应写回 config.yaml）。
func (c *Config) SaveConfig(filename string) error {
	saveConfigMu.Lock()
	defer saveConfigMu.Unlock()
	if os.Getenv("TG_DOWN_NO_CONFIG_WRITE") != "" {
		fmt.Fprintln(os.Stderr, "[配置] TG_DOWN_NO_CONFIG_WRITE 已设置，跳过配置写回")
		return nil
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	dir := filepath.Dir(filename)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(filename)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建配置临时文件失败: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	// CreateTemp 默认已经是 0600；仍显式设置，保证未来实现变化时配置里的凭据不会扩大权限。
	if err := temp.Chmod(FilePermission); err != nil {
		_ = temp.Close()
		return fmt.Errorf("设置配置文件权限失败: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("保存配置文件失败: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("同步配置文件失败: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("关闭配置文件失败: %w", err)
	}
	// 同目录 rename 保证读者只会看到旧文件或完整的新文件，写入中断不会留下半截 YAML。
	if err := os.Rename(tempName, filename); err != nil {
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}
