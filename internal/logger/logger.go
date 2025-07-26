package logger

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
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
	case "debug":
		logLevel = DEBUG
	case "info":
		logLevel = INFO
	case "warn":
		logLevel = WARN
	case "error":
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
func (l *Logger) formatMessage(level string, msg string) string {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf("[%s] %s: %s", timestamp, level, msg)
}

// Debug 调试日志
func (l *Logger) Debug(msg string, args ...interface{}) {
	if l.level <= DEBUG {
		message := fmt.Sprintf(msg, args...)
		l.logger.Println(l.formatMessage("DEBUG", message))
	}
}

// Info 信息日志
func (l *Logger) Info(msg string, args ...interface{}) {
	if l.level <= INFO {
		message := fmt.Sprintf(msg, args...)
		l.logger.Println(l.formatMessage("INFO", message))
	}
}

// Warn 警告日志
func (l *Logger) Warn(msg string, args ...interface{}) {
	if l.level <= WARN {
		message := fmt.Sprintf(msg, args...)
		l.logger.Println(l.formatMessage("WARN", message))
	}
}

// Error 错误日志
func (l *Logger) Error(msg string, args ...interface{}) {
	if l.level <= ERROR {
		message := fmt.Sprintf(msg, args...)
		l.logger.Println(l.formatMessage("ERROR", message))
	}
}

// Fatal 致命错误日志
func (l *Logger) Fatal(msg string, args ...interface{}) {
	message := fmt.Sprintf(msg, args...)
	l.logger.Println(l.formatMessage("FATAL", message))
	os.Exit(1)
}
