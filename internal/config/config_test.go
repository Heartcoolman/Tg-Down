package config

import (
	"os"
	"testing"
)

// 测试常量
const (
	TestAPIID = 12345
	TestAPIHash = "test_hash"
	TestPhone = "+1234567890"
)

func TestLoadConfig(t *testing.T) {
	// 设置测试环境变量
	if err := os.Setenv("API_ID", "12345"); err != nil {
		t.Fatalf("Failed to set API_ID: %v", err)
	}
	if err := os.Setenv("API_HASH", TestAPIHash); err != nil {
		t.Fatalf("Failed to set API_HASH: %v", err)
	}
	if err := os.Setenv("PHONE", TestPhone); err != nil {
		t.Fatalf("Failed to set PHONE: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("API_ID"); err != nil {
			t.Errorf("Failed to unset API_ID: %v", err)
		}
		if err := os.Unsetenv("API_HASH"); err != nil {
			t.Errorf("Failed to unset API_HASH: %v", err)
		}
		if err := os.Unsetenv("PHONE"); err != nil {
			t.Errorf("Failed to unset PHONE: %v", err)
		}
	}()

	config, err := LoadConfig()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if config.API.ID != TestAPIID {
		t.Errorf("Expected API ID %d, got %d", TestAPIID, config.API.ID)
	}
	if config.API.Hash != TestAPIHash {
		t.Errorf("Expected API Hash '%s', got %s", TestAPIHash, config.API.Hash)
	}
	if config.API.Phone != TestPhone {
		t.Errorf("Expected Phone '%s', got %s", TestPhone, config.API.Phone)
	}
}

func TestLoadConfigWithDefaults(t *testing.T) {
	if err := os.Setenv("API_ID", "12345"); err != nil {
		t.Fatalf("Failed to set API_ID: %v", err)
	}
	if err := os.Setenv("API_HASH", TestAPIHash); err != nil {
		t.Fatalf("Failed to set API_HASH: %v", err)
	}
	if err := os.Setenv("PHONE", TestPhone); err != nil {
		t.Fatalf("Failed to set PHONE: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("API_ID"); err != nil {
			t.Errorf("Failed to unset API_ID: %v", err)
		}
		if err := os.Unsetenv("API_HASH"); err != nil {
			t.Errorf("Failed to unset API_HASH: %v", err)
		}
		if err := os.Unsetenv("PHONE"); err != nil {
			t.Errorf("Failed to unset PHONE: %v", err)
		}
	}()

	config, err := LoadConfig()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// 检查默认值
	if config.Download.Path != DefaultDownloadPath {
		t.Errorf("Expected default download path '%s', got %s", DefaultDownloadPath, config.Download.Path)
	}
	if config.Download.MaxConcurrent != DefaultMaxConcurrent {
		t.Errorf("Expected default max concurrent %d, got %d", DefaultMaxConcurrent, config.Download.MaxConcurrent)
	}
	if config.Download.BatchSize != DefaultBatchSize {
		t.Errorf("Expected default batch size %d, got %d", DefaultBatchSize, config.Download.BatchSize)
	}
	if config.Log.Level != DefaultLogLevel {
		t.Errorf("Expected default log level '%s', got %s", DefaultLogLevel, config.Log.Level)
	}
	if config.Session.Dir != DefaultSessionDir {
		t.Errorf("Expected default session dir '%s', got %s", DefaultSessionDir, config.Session.Dir)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	// 清除所有相关环境变量
	if err := os.Unsetenv("API_ID"); err != nil {
		t.Errorf("Failed to unset API_ID: %v", err)
	}
	if err := os.Unsetenv("API_HASH"); err != nil {
		t.Errorf("Failed to unset API_HASH: %v", err)
	}
	if err := os.Unsetenv("PHONE"); err != nil {
		t.Errorf("Failed to unset PHONE: %v", err)
	}

	_, err := LoadConfig()
	if err == nil {
		t.Error("Expected error for missing required config, but got none")
	}
}

func TestSaveConfig(t *testing.T) {
	config := &Config{
		API: APIConfig{
			ID:    TestAPIID,
			Hash:  TestAPIHash,
			Phone: TestPhone,
		},
		Download: DownloadConfig{
			Path:          DefaultDownloadPath,
			MaxConcurrent: DefaultMaxConcurrent,
			BatchSize:     DefaultBatchSize,
		},
		Log: LogConfig{
			Level: DefaultLogLevel,
		},
		Session: SessionConfig{
			Dir: DefaultSessionDir,
		},
	}

	// 使用临时文件
	tempFile := t.TempDir() + "/test_config.yaml"

	err := config.SaveConfig(tempFile)
	if err != nil {
		t.Errorf("Failed to save config: %v", err)
	}

	// 验证文件是否存在
	if _, err := os.Stat(tempFile); os.IsNotExist(err) {
		t.Error("Config file was not created")
	}
}
