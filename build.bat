@echo off
echo 正在编译Telegram媒体下载器...
go build -o tg-down.exe cmd/main.go
if %ERRORLEVEL% EQU 0 (
    echo 编译成功！
    echo 运行程序: tg-down.exe
) else (
    echo 编译失败！
    pause
)