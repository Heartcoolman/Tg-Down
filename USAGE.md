# 使用指南

## 持久登录功能

本程序支持持久登录功能，首次登录成功后会自动保存会话信息，后续运行无需重复输入验证码。

### 首次登录

1. 运行程序：
   ```bash
   go run cmd/main.go
   ```

2. 程序会提示输入验证码：
   ```
   [INFO] Telegram群聊媒体下载器启动
   [INFO] 使用会话文件: sessions\session_+8613088042670.json
   [INFO] 未发现会话文件，需要进行首次登录
   [INFO] 正在连接到Telegram...
   [INFO] 开始认证流程...
   请输入验证码: 12345
   ```

3. 如果启用了两步验证，还需要输入密码：
   ```
   请输入两步验证密码: ******
   ```

4. 登录成功后，会话信息会自动保存到 `sessions` 目录

### 后续登录

再次运行程序时，会自动使用保存的会话信息：

```bash
go run cmd/main.go
```

输出示例：
```
[INFO] Telegram群聊媒体下载器启动
[INFO] 使用会话文件: sessions\session_+8613088042670.json
[INFO] 发现现有会话文件，将尝试自动登录
[INFO] 正在连接到Telegram...
[INFO] 成功连接到Telegram
```

### 清除会话

如果需要重新登录（例如更换账号），可以清除保存的会话：

```bash
go run cmd/main.go --clear-session
```

输出示例：
```
正在清除会话文件...
[INFO] 已清除会话文件: sessions\session_+8613088042670.json
会话文件已清除，下次启动将需要重新登录
```

### 会话文件管理

- 会话文件保存在 `sessions` 目录下
- 文件名格式：`session_手机号.json`
- 可以通过配置文件修改会话目录：
  ```yaml
  session:
    dir: "./my_sessions"
  ```
- 或通过环境变量：
  ```bash
  export SESSION_DIR=./my_sessions
  ```

### 安全注意事项

1. **保护会话文件**：会话文件包含登录凭据，请妥善保管
2. **定期清理**：如果不再使用某个账号，建议清除对应的会话文件
3. **备份重要数据**：建议定期备份会话文件，避免意外丢失
4. **权限控制**：确保会话目录只有当前用户可以访问

### 故障排除

#### 会话失效

如果遇到以下情况，会话可能已失效：
- 长时间未使用
- 账号在其他设备登录
- Telegram服务器端会话过期

解决方法：清除会话并重新登录
```bash
go run cmd/main.go --clear-session
go run cmd/main.go
```

#### 会话文件损坏

如果会话文件损坏，程序会自动检测并要求重新登录：
```
[ERROR] 会话文件损坏，需要重新登录
```

#### 权限问题

确保程序对会话目录有读写权限：
```bash
# Linux/macOS
chmod 755 sessions/
chmod 644 sessions/*.json

# Windows
# 通过文件属性设置适当的权限
```