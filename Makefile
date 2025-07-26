# Makefile for Telegram Media Downloader

.PHONY: build run clean deps help install

# 默认目标
all: build

# 构建程序
build:
	@echo "正在编译Telegram媒体下载器..."
	@go build -o tg-down cmd/main.go
	@echo "编译完成！"

# 运行程序
run: build
	@echo "正在运行程序..."
	@./tg-down

# 下载依赖
deps:
	@echo "正在下载依赖..."
	@go mod download
	@go mod tidy

# 清理构建文件
clean:
	@echo "正在清理构建文件..."
	@rm -f tg-down tg-down.exe
	@echo "清理完成！"

# 安装到系统路径
install: build
	@echo "正在安装到系统路径..."
	@sudo cp tg-down /usr/local/bin/
	@echo "安装完成！可以在任何位置使用 'tg-down' 命令"

# 创建配置文件
config:
	@if [ ! -f config.yaml ]; then \
		echo "创建配置文件..."; \
		cp config.yaml.example config.yaml; \
		echo "请编辑 config.yaml 文件设置您的API信息"; \
	else \
		echo "配置文件已存在"; \
	fi

# 显示帮助信息
help:
	@echo "可用的命令:"
	@echo "  make build   - 编译程序"
	@echo "  make run     - 编译并运行程序"
	@echo "  make deps    - 下载依赖"
	@echo "  make clean   - 清理构建文件"
	@echo "  make install - 安装到系统路径"
	@echo "  make config  - 创建配置文件"
	@echo "  make help    - 显示此帮助信息"

# 开发模式 - 监听文件变化并自动重新编译
dev:
	@echo "开发模式 - 监听文件变化..."
	@if command -v air > /dev/null; then \
		air; \
	else \
		echo "请安装 air 工具: go install github.com/cosmtrek/air@latest"; \
	fi