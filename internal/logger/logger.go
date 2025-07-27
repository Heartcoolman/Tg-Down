// Package logger provides logging utilities for Tg-Down application.
// It supports different log levels and formatted output with timestamps.
package logger

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// 日志级别常量
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// LogLevel 日志级别
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
)

// Logger 日志记录器
type Logger struct {
	level  LogLevel
	logger *log.Logger
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

// formatMessage 格式化日志消息
func (l *Logger) formatMessage(level, msg string) string {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("[%s] %s: %s", timestamp, level, msg)
}

// Debug 调试日志
func (l *Logger) Debug(msg string, args ...interface{}) {
	if l.level <= DEBUG {
		fmt.Println(l.formatMessage("DEBUG", fmt.Sprintf(msg, args...)))
	}
}

// Info 信息日志
func (l *Logger) Info(msg string, args ...interface{}) {
	if l.level <= INFO {
		fmt.Println(l.formatMessage("INFO", fmt.Sprintf(msg, args...)))
	}
}

// Warn 警告日志
func (l *Logger) Warn(msg string, args ...interface{}) {
	if l.level <= WARN {
		fmt.Println(l.formatMessage("WARN", fmt.Sprintf(msg, args...)))
	}
}

// Error 错误日志
func (l *Logger) Error(msg string, args ...interface{}) {
	if l.level <= ERROR {
		fmt.Println(l.formatMessage("ERROR", fmt.Sprintf(msg, args...)))
	}
}

// Fatal 致命错误日志
func (l *Logger) Fatal(msg string, args ...interface{}) {
	fmt.Println(l.formatMessage("FATAL", fmt.Sprintf(msg, args...)))
	os.Exit(1)
}
