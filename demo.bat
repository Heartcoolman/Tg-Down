@echo off
chcp 65001 >nul
echo === Telegram群聊媒体下载器 - 持久登录功能演示 ===
echo.

echo 1. 检查当前会话状态...
if exist "sessions" (
    dir /b sessions 2>nul | findstr /r ".*" >nul
    if not errorlevel 1 (
        echo    ✓ 发现现有会话文件:
        dir sessions
        echo.
        echo    如果要重新登录，请运行: go run cmd/main.go --clear-session
    ) else (
        echo    ✗ 未发现会话文件，首次运行需要登录认证
    )
) else (
    echo    ✗ 会话目录不存在，首次运行需要登录认证
)

echo.
echo 2. 可用命令:
echo    启动程序:     go run cmd/main.go
echo    清除会话:     go run cmd/main.go --clear-session
echo    编译程序:     go build -o tg-down.exe cmd/main.go
echo.

echo 3. 配置文件:
echo    主配置:       config.yaml
echo    环境变量:     .env
echo    会话目录:     sessions/
echo.

echo 4. 功能特性:
echo    ✓ 持久登录 - 首次登录后自动保存会话
echo    ✓ 自动重连 - 后续启动无需重新输入验证码
echo    ✓ 会话管理 - 支持清除和重置会话
echo    ✓ 多账号支持 - 基于手机号区分不同会话
echo    ✓ 安全存储 - 会话文件本地加密保存
echo.

echo === 开始使用 ===
echo 运行以下命令启动程序:
echo go run cmd/main.go
echo.
pause