package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// 测试配置加载功能
	// 设置测试环境变量
	if err := os.Setenv("API_ID", "12345"); err != nil {
		t.Fatalf("Failed to set API_ID: %v", err)
	}
	if err := os.Setenv("API_HASH", "test_hash"); err != nil {
		t.Fatalf("Failed to set API_HASH: %v", err)
	}
	if err := os.Setenv("PHONE", "+1234567890"); err != nil {
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

	if config.API.ID != 12345 {
		t.Errorf("Expected API ID 12345, got %d", config.API.ID)
	}
	if config.API.Hash != "test_hash" {
		t.Errorf("Expected API Hash 'test_hash', got %s", config.API.Hash)
	}
	if config.API.Phone != "+1234567890" {
		t.Errorf("Expected Phone '+1234567890', got %s", config.API.Phone)
	}
}

func TestLoadConfigWithDefaults(t *testing.T) {
	// 测试默认值设置
	if err := os.Setenv("API_ID", "12345"); err != nil {
		t.Fatalf("Failed to set API_ID: %v", err)
	}
	if err := os.Setenv("API_HASH", "test_hash"); err != nil {
		t.Fatalf("Failed to set API_HASH: %v", err)
	}
	if err := os.Setenv("PHONE", "+1234567890"); err != nil {
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
	if config.Download.Path != "./downloads" {
		t.Errorf("Expected default download path './downloads', got %s", config.Download.Path)
	}
	if config.Download.MaxConcurrent != 5 {
		t.Errorf("Expected default max concurrent 5, got %d", config.Download.MaxConcurrent)
	}
	if config.Download.BatchSize != 100 {
		t.Errorf("Expected default batch size 100, got %d", config.Download.BatchSize)
	}
	if config.Log.Level != "info" {
		t.Errorf("Expected default log level 'info', got %s", config.Log.Level)
	}
	if config.Session.Dir != "./sessions" {
		t.Errorf("Expected default session dir './sessions', got %s", config.Session.Dir)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	// 测试缺少必要配置的情况
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
	// 测试保存配置
	config := &Config{
		API: APIConfig{
			ID:    12345,
			Hash:  "test_hash",
			Phone: "+1234567890",
		},
		Download: DownloadConfig{
			Path:          "./downloads",
			MaxConcurrent: 5,
			BatchSize:     100,
		},
		Log: LogConfig{
			Level: "info",
		},
		Session: SessionConfig{
			Dir: "./sessions",
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
