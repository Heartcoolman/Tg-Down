# Makefile for Telegram Media Downloader (TDLib engine)

.PHONY: all build run clean deps help install config tdlib dev

# TDLib 安装前缀（scripts/install-tdlib.sh 默认装到这里）
TDLIB_PREFIX ?= $(HOME)/.tdlib
# OpenSSL 前缀（macOS Homebrew: openssl@3；Linux 通常留空）
OPENSSL_PREFIX ?= $(shell command -v brew >/dev/null 2>&1 && brew --prefix openssl@3 2>/dev/null)

# go-tdlib master 起 darwin 不再内置链接指令（linux 默认静态链接由绑定自带），
# macOS 需显式给出 TDLib 静态库列表
TDLIB_STATIC_LIBS = -ltdjson_static -ltdjson_private -ltdclient -ltdcore -ltde2e -ltdmtproto -ltdactor -ltdapi -ltddb -ltdsqlite -ltdnet -ltdutils -lstdc++ -lssl -lcrypto -ldl -lz -lm
UNAME_S := $(shell uname -s)

export CGO_ENABLED = 1
export CGO_CFLAGS = -I$(TDLIB_PREFIX)/include $(if $(OPENSSL_PREFIX),-I$(OPENSSL_PREFIX)/include,)
export CGO_LDFLAGS = -L$(TDLIB_PREFIX)/lib -Wl,-rpath,$(TDLIB_PREFIX)/lib $(if $(OPENSSL_PREFIX),-L$(OPENSSL_PREFIX)/lib,) $(if $(filter Darwin,$(UNAME_S)),$(TDLIB_STATIC_LIBS),)

# 默认目标
all: build

# 构建 TDLib（首次需要，约 30-60 分钟）
tdlib:
	@echo "正在构建 TDLib 到 $(TDLIB_PREFIX) ..."
	@bash scripts/install-tdlib.sh

# 构建程序（需先 make tdlib 安装 libtdjson）
build:
	@echo "正在编译Telegram媒体下载器 (CGo + TDLib)..."
	@go build -o tg-down ./cmd
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
	@echo "  make tdlib   - 构建并安装 TDLib (首次必需)"
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
		echo "请安装 air 工具: go install github.com/air-verse/air@latest"; \
	fi
