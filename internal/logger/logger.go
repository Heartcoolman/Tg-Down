// Package logger provides logging utilities for Tg-Down application.
// It supports different log levels and formatted output with timestamps.
package logger

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// 日志级别常量
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	// ExitCodeFatal is the exit code for fatal errors
	ExitCodeFatal = 1
)

// LogLevel 日志级别
type LogLevel int

const (
	// DEBUG is the log level for debug messages
	DEBUG LogLevel = iota
	// INFO is the log level for informational messages
	INFO
	// WARN is the log level for warning messages
	WARN
	// ERROR is the log level for error messages
	ERROR
)

// Logger 日志记录器
type Logger struct {
	level  LogLevel
	logger *log.Logger
	mu     sync.RWMutex
	hook   func(level, msg string)
}

// New 创建新的日志记录器
func New(level string) *Logger {
	var logLevel LogLevel
	switch strings.ToLower(level) {
	case LevelDebug:
		logLevel = DEBUG
	case LevelInfo:
		logLevel = INFO
	case LevelWarn:
		logLevel = WARN
	case LevelError:
		logLevel = ERROR
	default:
		logLevel = INFO
	}

	return &Logger{
		level:  logLevel,
		logger: log.New(os.Stdout, "", 0),
	}
}

// SetHook 注册日志回调（如 Web 端 SSE 广播）。回调必须非阻塞且不得再调用本 logger。
func (l *Logger) SetHook(hook func(level, msg string)) {
	l.mu.Lock()
	l.hook = hook
	l.mu.Unlock()
}

// emit 将已格式化的日志转发给回调
func (l *Logger) emit(level, msg string) {
	l.mu.RLock()
	hook := l.hook
	l.mu.RUnlock()
	if hook != nil {
		hook(level, msg)
	}
}

// formatMessage 格式化日志消息
func (l *Logger) formatMessage(level, msg string) string {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("[%s] %s: %s", timestamp, level, msg)
}

// output 按级别输出并广播
func (l *Logger) output(minLevel LogLevel, level, msg string, args ...interface{}) {
	if l.level > minLevel {
		return
	}
	formatted := fmt.Sprintf(msg, args...)
	fmt.Println(l.formatMessage(level, formatted))
	l.emit(level, formatted)
}

// Debug 调试日志
func (l *Logger) Debug(msg string, args ...interface{}) {
	l.output(DEBUG, "DEBUG", msg, args...)
}

// Info 信息日志
func (l *Logger) Info(msg string, args ...interface{}) {
	l.output(INFO, "INFO", msg, args...)
}

// Warn 警告日志
func (l *Logger) Warn(msg string, args ...interface{}) {
	l.output(WARN, "WARN", msg, args...)
}

// Error 错误日志
func (l *Logger) Error(msg string, args ...interface{}) {
	l.output(ERROR, "ERROR", msg, args...)
}

// Fatal 致命错误日志
func (l *Logger) Fatal(msg string, args ...interface{}) {
	formatted := fmt.Sprintf(msg, args...)
	fmt.Println(l.formatMessage("FATAL", formatted))
	l.emit("FATAL", formatted)
	os.Exit(ExitCodeFatal)
}
