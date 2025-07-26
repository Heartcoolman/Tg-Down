@echo off
echo 正在运行Telegram媒体下载器...
if exist tg-down.exe (
    tg-down.exe
) else (
    echo 程序未找到，正在编译...
    call build.bat
    if exist tg-down.exe (
        tg-down.exe
    ) else (
        echo 编译失败，无法运行程序
        pause
    )
)