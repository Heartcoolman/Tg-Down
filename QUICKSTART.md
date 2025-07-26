# 快速开始指南

## 1. 获取Telegram API凭据

1. 访问 https://my.telegram.org
2. 使用您的手机号登录
3. 点击 "API development tools"
4. 填写应用信息创建新应用
5. 记录下 `api_id` 和 `api_hash`

## 2. 配置应用

### 方法一：使用YAML配置文件
```bash
# 复制配置模板
copy config.yaml.example config.yaml

# 编辑配置文件
notepad config.yaml
```

在 `config.yaml` 中填入您的信息：
```yaml
api:
  id: 你的API_ID
  hash: "你的API_HASH"
  phone: "你的手机号"
```

### 方法二：使用环境变量
```bash
# 复制环境变量模板
copy .env.example .env

# 编辑环境变量文件
notepad .env
```

## 3. 运行程序

### Windows用户
```bash
# 直接运行
run.bat

# 或者手动编译运行
build.bat
tg-down.exe
```

### 测试配置
```bash
# 测试配置是否正确
test-config.exe
```

## 4. 使用流程

1. **首次运行**：程序会要求输入验证码进行登录
2. **选择群聊**：从列表中选择要下载媒体的群聊
3. **选择模式**：
   - 模式1：只下载历史媒体文件
   - 模式2：只监控新消息
   - 模式3：下载历史 + 监控新消息（推荐）

## 5. 文件存储

下载的文件会保存在 `downloads/` 目录下，按聊天分组：
```
downloads/
├── chat_123456789/
│   ├── photo_1_xxx.jpg
│   ├── video_2_xxx.mp4
│   └── document_3_xxx.pdf
└── chat_987654321/
    └── ...
```

## 6. 常见问题

**Q: 认证失败怎么办？**
A: 检查API_ID、API_HASH和手机号是否正确

**Q: 下载速度慢怎么办？**
A: 可以在配置中增加 `max_concurrent` 值

**Q: 程序崩溃怎么办？**
A: 检查网络连接和磁盘空间，查看错误日志

**Q: 如何下载特定群组？**
A: 在配置文件中设置 `target_id` 为群组ID

## 7. 高级配置

```yaml
download:
  path: "./downloads"      # 下载路径
  max_concurrent: 5        # 最大并发下载数
  batch_size: 100         # 批量处理大小

log:
  level: "info"           # 日志级别: debug, info, warn, error
```

## 8. 注意事项

- 确保有足够的磁盘空间
- 保持网络连接稳定
- 不要泄露API凭据
- 遵守Telegram的使用条款