package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tg-down/internal/logger"
)

const (
	// TestFilePermission is the permission mode for creating test files
	TestFilePermission = 0600
	// TestAPIID is the test API ID
	TestAPIID = 12345
	// TestAPIHash is the test API hash
	TestAPIHash = "test_hash"
	// TestPhone is the test phone number
	TestPhone = "+1234567890"
)

func TestNew(t *testing.T) {
	// 测试创建会话管理器
	tempDir := t.TempDir()
	testLogger := logger.New("debug")
	manager := New(tempDir, testLogger)

	if manager.sessionDir != tempDir {
		t.Errorf("Expected session dir %s, got %s", tempDir, manager.sessionDir)
	}
}

func TestGetSessionStorage(t *testing.T) {
	// 测试获取会话存储
	tempDir := t.TempDir()
	testLogger := logger.New("debug")
	manager := New(tempDir, testLogger)
	phone := TestPhone

	storage := manager.GetSessionStorage(phone)
	if storage == nil {
		t.Error("Expected non-nil storage")
	}

	// 验证会话目录是否被创建
	if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		t.Error("Session directory should be created")
	}
}

func TestHasValidSession(t *testing.T) {
	// 测试检查会话文件是否存在
	tempDir := t.TempDir()
	testLogger := logger.New("debug")
	manager := New(tempDir, testLogger)
	phone := TestPhone

	// 初始状态应该不存在
	if manager.HasValidSession(phone) {
		t.Error("Session should not exist initially")
	}

	// 创建一个空的会话文件
	sessionPath := filepath.Join(tempDir, "session_+1234567890.json")

	// 验证路径安全性
	if !filepath.IsAbs(sessionPath) {
		sessionPath = filepath.Clean(sessionPath)
	}
	if strings.Contains(sessionPath, "..") {
		t.Fatalf("Invalid session path detected: %s", sessionPath)
	}

	file, err := os.Create(sessionPath)
	if err != nil {
		t.Fatalf("Failed to create test session file: %v", err)
	}
	closeErr := file.Close()
	if closeErr != nil {
		t.Fatalf("Failed to close test session file: %v", closeErr)
	}

	// 空文件应该返回false
	if manager.HasValidSession(phone) {
		t.Error("Empty session file should not be valid")
	}

	// 写入一些内容
	writeErr := os.WriteFile(sessionPath, []byte("test content"), TestFilePermission)
	if writeErr != nil {
		t.Fatalf("Failed to write test session file: %v", writeErr)
	}

	// 现在应该存在
	if !manager.HasValidSession(phone) {
		t.Error("Session should exist after creation with content")
	}
}

func TestClearSession(t *testing.T) {
	// 测试清除会话文件
	tempDir := t.TempDir()
	testLogger := logger.New("debug")
	manager := New(tempDir, testLogger)
	phone := TestPhone

	// 创建一个会话文件
	sessionPath := filepath.Join(tempDir, "session_+1234567890.json")
	err := os.WriteFile(sessionPath, []byte("test content"), TestFilePermission)
	if err != nil {
		t.Fatalf("Failed to create test session file: %v", err)
	}

	// 验证文件存在
	if !manager.HasValidSession(phone) {
		t.Error("Session should exist before clearing")
	}

	// 清除会话
	err = manager.ClearSession(phone)
	if err != nil {
		t.Errorf("Failed to clear session: %v", err)
	}

	// 验证文件已被删除
	if manager.HasValidSession(phone) {
		t.Error("Session should not exist after clearing")
	}
}

func TestCreateClientWithSession(t *testing.T) {
	// 测试创建带会话的客户端
	tempDir := t.TempDir()
	testLogger := logger.New("debug")
	manager := New(tempDir, testLogger)

	client := manager.CreateClientWithSession(TestAPIID, TestAPIHash, TestPhone)
	if client == nil {
		t.Error("Expected non-nil client")
	}
}
