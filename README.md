# Telegram群聊媒体下载器

一个基于Go语言的Telegram群聊视频图片下载脚本，支持实时监控新消息和批量下载历史媒体文件。

## 🚀 快速开始

### 第一步：获取API凭据
1. 访问 [https://my.telegram.org](https://my.telegram.org) 并登录
2. 创建新应用，获取 `api_id` 和 `api_hash`

### 第二步：配置和运行
```bash
# 1. 复制配置文件
cp config.yaml.example config.yaml

# 2. 编辑config.yaml，填入你的API信息
# 3. 运行程序
go run cmd/main.go

# 4. 首次运行需要输入验证码登录
# 5. 后续运行会自动使用保存的会话，无需重复登录
```

### 演示脚本
```bash
# Linux/macOS
./demo.sh

# Windows
demo.bat
```

## 功能特性

- 🚀 **实时监控**: 自动监控群聊新消息并下载媒体文件
- 📚 **历史下载**: 批量下载群聊历史消息中的媒体文件
- 🔄 **并发下载**: 支持多线程并发下载，提高下载效率
- 📊 **下载统计**: 实时显示下载进度和统计信息
- 🎯 **智能去重**: 自动跳过已下载的文件
- 📁 **分类存储**: 按聊天分组存储下载的文件
- 🔐 **持久登录**: 首次登录后保存会话，无需重复认证
- ⚙️ **灵活配置**: 支持YAML配置文件和环境变量配置

## 安装和配置

### 方式一：使用预编译版本（推荐）

1. **下载预编译版本**：
   - 访问 [GitHub Releases](https://github.com/Heartcoolman/Tg-Down/releases) 页面
   - 下载适合您操作系统的预编译版本
   - 解压到任意目录

2. **克隆项目配置**：
   ```bash
   # 克隆项目到本地获取配置文件
   git clone https://github.com/Heartcoolman/Tg-Down.git
   
   # 将预编译的可执行文件放入项目文件夹
   # 这样可以使用项目中的配置文件模板和脚本
   ```

3. **配置和运行**：
   ```bash
   # 复制配置文件模板
   cp config.yaml.example config.yaml
   
   # 编辑配置文件，填入API信息
   # 直接运行预编译版本
   ./tg-down  # Linux/macOS
   # 或
   tg-down.exe  # Windows
   ```

### 方式二：从源码编译

### 1. 获取Telegram API凭据

1. 访问 [https://my.telegram.org](https://my.telegram.org)
2. 登录你的Telegram账号
3. 点击 "API development tools"
4. 创建新应用，获取 `api_id` 和 `api_hash`

### 2. 配置应用

复制配置文件模板：
```bash
cp config.yaml.example config.yaml
cp .env.example .env
```

编辑 `config.yaml` 文件：
```yaml
api:
  id: 你的API_ID
  hash: "你的API_HASH"
  phone: "你的手机号"

download:
  path: "./downloads"
  max_concurrent: 5
  batch_size: 100

session:
  dir: "./sessions"

log:
  level: "info"
```

或者设置环境变量：
```bash
export API_ID=你的API_ID
export API_HASH=你的API_HASH
export PHONE=你的手机号
```

### 3. 安装依赖

```bash
go mod download
```

### 4. 编译运行

```bash
# 编译
go build -o tg-down cmd/main.go

# 运行
./tg-down
```

或者直接运行：
```bash
go run cmd/main.go
```

## 使用说明

### 首次运行

1. 运行程序后，会要求输入验证码进行登录
2. 如果启用了两步验证，还需要输入密码
3. 登录成功后，会话信息会自动保存到 `sessions` 目录
4. 程序会显示可用的聊天列表，选择要下载的群聊
5. 选择操作模式：
   - 模式1：只下载历史媒体文件
   - 模式2：只监控新消息
   - 模式3：下载历史媒体文件 + 监控新消息

### 后续运行

- 程序会自动使用保存的会话信息登录，无需重新输入验证码
- 如果需要重新登录，可以使用 `--clear-session` 参数清除会话：
  ```bash
  ./tg-down --clear-session
  ```

### 配置选项

| 配置项 | 说明 | 默认值 |
|--------|------|--------|
| `api.id` | Telegram API ID | 必填 |
| `api.hash` | Telegram API Hash | 必填 |
| `api.phone` | 手机号 | 必填 |
| `download.path` | 下载路径 | `./downloads` |
| `download.max_concurrent` | 最大并发下载数 | `5` |
| `download.batch_size` | 批量处理大小 | `100` |
| `session.dir` | 会话文件保存目录 | `./sessions` |
| `chat.target_id` | 目标群组ID（可选） | `0` |
| `log.level` | 日志级别 | `info` |

### 文件组织结构

下载的文件会按以下结构组织：
```
downloads/
├── chat_123456789/          # 群聊ID
│   ├── photo_1_xxx.jpg      # 图片文件
│   ├── video_2_xxx.mp4      # 视频文件
│   └── document_3_xxx.pdf   # 文档文件
└── chat_987654321/
    └── ...
```

## 开发说明

### 项目结构

```
tg-down/
├── .github/
│   ├── workflows/           # GitHub Actions工作流
│   └── dependabot.yml      # Dependabot配置
├── cmd/
│   └── main.go              # 主程序入口
├── internal/
│   ├── config/              # 配置管理
│   ├── logger/              # 日志记录
│   ├── session/             # 会话管理
│   ├── downloader/          # 下载器
│   └── telegram/            # Telegram客户端
├── sessions/                # 会话文件目录
├── downloads/               # 下载目录
├── config.yaml.example     # 配置文件模板
├── .env.example            # 环境变量模板
├── .golangci.yml           # 代码质量检查配置
├── go.mod                  # Go模块文件
└── README.md               # 说明文档
```

### 主要模块

- **config**: 配置文件和环境变量管理
- **logger**: 分级日志记录
- **session**: 持久登录会话管理
- **downloader**: 并发媒体文件下载器
- **telegram**: Telegram API客户端封装

### CI/CD 流水线

项目使用GitHub Actions实现完全自动化的CI/CD流水线：

#### 🔄 持续集成 (CI)
- **代码质量检查**: 使用golangci-lint进行代码规范检查
- **自动化测试**: 运行单元测试和集成测试
- **安全扫描**: 使用Gosec进行安全漏洞扫描
- **依赖管理**: 自动提交Go依赖信息到GitHub依赖图

#### 📦 自动化发布 (CD)
- **智能版本管理**: 根据提交信息自动确定版本号提升类型
  - `breaking`/`major` 关键词 → 主版本号提升 (v1.0.0 → v2.0.0)
  - `feat`/`feature`/`minor` 关键词 → 次版本号提升 (v1.0.0 → v1.1.0)
  - 其他提交 → 补丁版本号提升 (v1.0.0 → v1.0.1)
- **完整打包**: 自动创建包含所有依赖的完整发布包
  - Windows: `tg-down-windows-amd64.zip` (包含 .exe、启动脚本、配置文件)
  - Linux: `tg-down-linux-amd64.tar.gz` (包含可执行文件、启动脚本、配置文件)
  - macOS: `tg-down-macos-amd64.tar.gz` (包含可执行文件、启动脚本、配置文件)
- **开箱即用**: 每个发布包都包含：
  - 编译好的可执行文件
  - 预配置的 config.yaml 文件
  - 一键启动脚本 (start.bat/start.sh)
  - 详细的配置说明文档
  - 许可证文件
- **多平台构建**: 支持Linux、Windows、macOS (AMD64/ARM64)
- **校验和生成**: 为所有构建产物生成SHA256校验和

#### 🚀 一键发布流程
1. **推送代码到 main 分支** → 自动检测是否需要发布新版本
2. **自动创建版本标签** → 根据提交信息智能确定版本号
3. **触发发布流程** → 多平台编译和打包
4. **创建 GitHub Release** → 包含完整的发布说明和下载链接
5. **用户下载使用** → 解压即可运行，无需额外配置

#### 🤖 依赖管理
- **Dependabot**: 自动检测并更新Go模块依赖
- **安全更新**: 自动接收依赖安全漏洞警报
- **依赖图**: 可视化项目依赖关系

### 开发工作流

1. **本地开发**:
   ```bash
   # 安装依赖
   go mod download
   
   # 运行代码检查
   golangci-lint run
   
   # 运行测试
   go test ./...
   
   # 本地构建
   go build -o tg-down cmd/main.go
   ```

2. **提交代码**:
   - 推送到分支会触发CI检查
   - Pull Request会运行完整的测试套件
   - 合并到主分支会更新依赖图

3. **自动化发布** (推荐):
   ```bash
   # 直接推送到 main 分支，系统会自动处理版本发布
   git push origin main
   
   # 系统会根据提交信息自动：
   # 1. 确定版本号提升类型
   # 2. 创建版本标签
   # 3. 触发多平台构建和打包
   # 4. 创建 GitHub Release
   ```

4. **手动发布** (可选):
   ```bash
   # 手动创建版本标签
   git tag v1.0.0
   git push origin v1.0.0
   
   # 自动触发多平台构建和发布
   ```

#### 💡 提交信息规范

为了让自动版本管理正确工作，建议使用以下提交信息格式：

- **主版本更新** (v1.0.0 → v2.0.0):
  ```
  breaking: 重构API接口，不兼容旧版本
  major: 重大架构变更
  ```

- **次版本更新** (v1.0.0 → v1.1.0):
  ```
  feat: 添加新的下载模式
  feature: 支持视频压缩功能
  minor: 增加配置选项
  ```

- **补丁版本更新** (v1.0.0 → v1.0.1):
  ```
  fix: 修复下载失败问题
  docs: 更新文档
  style: 代码格式化
  refactor: 重构代码结构
  ```

## 注意事项

1. **API限制**: Telegram对API调用有频率限制，程序已内置适当的延迟
2. **存储空间**: 确保有足够的磁盘空间存储下载的媒体文件
3. **网络稳定**: 建议在稳定的网络环境下运行
4. **账号安全**: 妥善保管API凭据，不要泄露给他人

## 故障排除

### 常见问题

1. **认证失败**: 检查API_ID、API_HASH和手机号是否正确
2. **下载失败**: 检查网络连接和磁盘空间
3. **权限错误**: 确保对下载目录有写入权限

### 日志级别

- `debug`: 详细的调试信息
- `info`: 一般信息（默认）
- `warn`: 警告信息
- `error`: 错误信息

## 许可证

本项目采用 MIT 许可证，详见 LICENSE 文件。

## 贡献

欢迎提交Issue和Pull Request来改进这个项目。

---

## 📋 更新日志

<<<<<<< HEAD
### v1.3.0 (2025-07-29)
- 🚀 **完全自动化发布**：实现 git push 后自动打包发布的完整流程
- 📦 **开箱即用打包**：每个发布包包含所有依赖文件，下载后可直接运行
- 🤖 **智能版本管理**：根据提交信息自动确定版本号提升类型
- 📋 **完整发布包**：包含可执行文件、配置文件、启动脚本和说明文档
- 🎯 **一键启动**：Windows 用户双击 start.bat，Linux/macOS 用户运行 ./start.sh
- 📚 **详细说明文档**：每个平台包含专门的配置说明和使用指南
- ⚡ **自动化工作流**：推送到 main 分支自动触发版本检测和发布流程

### v1.2.0 (2025-07-29)
- 🔧 **代码质量改进**：重构 main 函数，降低圈复杂度
- 🔧 **消除魔法数字**：用描述性常量替换所有魔法数字
- 🔧 **Go 规范修复**：为导出常量添加符合 Go 规范的注释
- 🔒 **安全性增强**：替换弱随机数生成器，添加路径验证
- 🎨 **代码格式化**：使用 gofmt 格式化所有代码
- ⚙️ **GitHub Actions 优化**：更新工作流配置，修复已弃用的配置项
- 📁 **项目结构优化**：修复文档结构，确保 README.md 位于正确位置
- ✅ **CI/CD 改进**：所有 golangci-lint 检查通过，无错误和警告

### v1.1.0 (2025-07-27)
- ✨ **性能优化**：改进下载器并发处理机制
- 🔧 **配置增强**：支持更多配置选项和环境变量
- 🐛 **Bug 修复**：修复会话管理和错误处理问题
- 📚 **文档完善**：添加详细的使用说明和故障排除指南

### v1.0.0 (2025-07-27)
- 🎉 **初始版本发布**
- ✨ **实时监控**：支持实时监控群聊新消息并下载媒体文件
- ✨ **历史下载**：支持批量下载群聊历史消息中的媒体文件
- ✨ **并发下载**：支持多线程并发下载，提高下载效率
- ✨ **智能去重**：自动跳过已下载的文件
- ✨ **分类存储**：按聊天分组存储下载的文件
- ✨ **持久登录**：首次登录后保存会话，无需重复认证
- ✨ **灵活配置**：支持YAML配置文件和环境变量配置
- 🔧 **CI/CD 流水线**：完整的GitHub Actions自动化流水线
- 📚 **完整文档**：详细的安装、配置和使用说明
