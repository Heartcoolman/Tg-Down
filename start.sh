#!/bin/bash

echo "Telegram群聊媒体下载器"
echo "===================="

# 检查Go是否安装
if ! command -v go &> /dev/null; then
    echo "错误: 未找到Go语言环境，请先安装Go"
    exit 1
fi

# 检查配置文件
if [ ! -f "config.yaml" ] && [ ! -f ".env" ]; then
    echo "警告: 未找到配置文件"
    echo "请复制并编辑配置文件:"
    echo "  cp config.yaml.example config.yaml"
    echo "  或者"
    echo "  cp .env.example .env"
    echo ""
fi

# 下载依赖
echo "正在下载依赖..."
go mod download

# 编译程序
echo "正在编译程序..."
go build -o tg-down cmd/main.go

if [ $? -eq 0 ]; then
    echo "编译成功！"
    echo "运行程序: ./tg-down"
    echo ""
    
    # 询问是否立即运行
    read -p "是否立即运行程序? (y/n): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        ./tg-down
    fi
else
    echo "编译失败！"
    exit 1
fi