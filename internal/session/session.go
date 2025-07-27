package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"tg-down/internal/logger"
)

// Manager 会话管理器
type Manager struct {
	sessionDir string
	logger     *logger.Logger
}

// New 创建新的会话管理器
func New(sessionDir string, logger *logger.Logger) *Manager {
	return &Manager{
		sessionDir: sessionDir,
		logger:     logger,
	}
}

// GetSessionStorage 获取会话存储
func (m *Manager) GetSessionStorage(phone string) session.Storage {
	// 确保会话目录存在
	if err := os.MkdirAll(m.sessionDir, 0750); err != nil {
		m.logger.Error("创建会话目录失败: %v", err)
		return nil
	}

	// 生成会话文件名（基于手机号）
	sessionFile := filepath.Join(m.sessionDir, fmt.Sprintf("session_%s.json", phone))

	// 创建文件会话存储
	storage := &session.FileStorage{
		Path: sessionFile,
	}

	m.logger.Info("使用会话文件: %s", sessionFile)
	return storage
}

// HasValidSession 检查是否有有效的会话文件
func (m *Manager) HasValidSession(phone string) bool {
	sessionFile := filepath.Join(m.sessionDir, fmt.Sprintf("session_%s.json", phone))

	// 检查文件是否存在
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return false
	}

	// 检查文件是否为空
	info, err := os.Stat(sessionFile)
	if err != nil {
		return false
	}

	return info.Size() > 0
}

// CreateClientWithSession 创建带会话的Telegram客户端
func (m *Manager) CreateClientWithSession(apiID int, apiHash, phone string) *telegram.Client {
	storage := m.GetSessionStorage(phone)
	if storage == nil {
		m.logger.Error("无法创建会话存储")
		return nil
	}

	options := telegram.Options{
		SessionStorage: storage,
	}

	client := telegram.NewClient(apiID, apiHash, options)
	return client
}

// ClearSession 清除会话文件
func (m *Manager) ClearSession(phone string) error {
	sessionFile := filepath.Join(m.sessionDir, fmt.Sprintf("session_%s.json", phone))

	if err := os.Remove(sessionFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除会话文件失败: %w", err)
	}

	m.logger.Info("已清除会话文件: %s", sessionFile)
	return nil
}
