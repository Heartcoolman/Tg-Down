# GitHub Actions 工作流说明

本项目使用GitHub Actions实现完整的CI/CD流水线，包括代码质量检查、自动化测试、安全扫描、多平台构建和依赖管理。

## 工作流概览

### 🔄 CI 流水线 (`ci.yml`)

**触发条件**:
- 推送到 `main`, `master`, `develop` 分支
- 针对 `main`, `master` 分支的Pull Request

**包含的作业**:

#### 1. 测试作业 (test)
- **环境**: Ubuntu Latest, Go 1.21
- **步骤**:
  - 检出代码
  - 设置Go环境
  - 下载并验证依赖
  - 运行 `go vet` 静态分析
  - 运行 `go test` 单元测试
  - 构建应用程序
  - 上传构建产物

#### 2. 安全扫描 (security)
- **工具**: Gosec
- **功能**: 扫描Go代码中的安全漏洞
- **输出**: SARIF格式的安全报告

#### 3. 依赖提交 (dependency-submission)
- **工具**: go-dependency-submission
- **功能**: 将Go模块依赖信息提交到GitHub依赖图
- **好处**: 启用Dependabot安全警报和依赖可视化

### 📦 发布流水线 (`release.yml`)

**触发条件**:
- 推送版本标签 (格式: `v*`, 如 `v1.0.0`)

**构建矩阵**:
- **Linux**: AMD64, ARM64
- **Windows**: AMD64
- **macOS**: AMD64, ARM64

**发布流程**:
1. 多平台并行构建
2. 生成SHA256校验和
3. 创建GitHub Release
4. 上传所有构建产物和校验和文件

### 🔍 代码质量检查 (`code-quality.yml`)

**触发条件**:
- 推送到任何分支
- Pull Request

**检查项目**:
1. **Linting**: 使用golangci-lint进行代码规范检查
2. **格式化**: 检查代码是否符合gofmt标准
3. **导入整理**: 验证goimports格式
4. **模块一致性**: 检查go.mod和go.sum的一致性

### 🤖 依赖管理 (`dependabot.yml`)

**自动化功能**:
- **Go模块**: 每周检查依赖更新
- **GitHub Actions**: 每周检查Action版本更新
- **自动PR**: 创建依赖更新的Pull Request
- **标签和审核**: 自动添加标签并指定审核者

## 配置文件说明

### `.golangci.yml`
golangci-lint的配置文件，定义了：
- 启用的linter列表
- 各linter的具体配置
- 排除规则（如测试文件的某些检查）
- 输出格式和超时设置

### `dependabot.yml`
Dependabot的配置文件，包含：
- 包管理器类型（go modules, github-actions）
- 更新频率（每周）
- 提交消息格式
- 审核者和标签设置

## 使用指南

### 本地开发

1. **安装工具**:
   ```bash
   # 安装golangci-lint
   go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
   
   # 安装goimports
   go install golang.org/x/tools/cmd/goimports@latest
   ```

2. **运行检查**:
   ```bash
   # 代码检查
   golangci-lint run
   
   # 格式化检查
   gofmt -l .
   
   # 导入整理
   goimports -l .
   
   # 模块验证
   go mod tidy
   go mod verify
   ```

### 发布新版本

1. **准备发布**:
   ```bash
   # 确保代码已提交
   git add .
   git commit -m "准备发布 v1.0.0"
   git push origin main
   ```

2. **创建标签**:
   ```bash
   # 创建版本标签
   git tag v1.0.0
   git push origin v1.0.0
   ```

3. **自动构建**: 推送标签后，GitHub Actions会自动：
   - 构建多平台二进制文件
   - 生成校验和
   - 创建GitHub Release
   - 上传所有产物

### 依赖管理

1. **查看依赖图**: 在GitHub仓库的"Insights" > "Dependency graph"中查看
2. **安全警报**: 在"Security" > "Dependabot alerts"中查看安全漏洞
3. **自动更新**: Dependabot会自动创建依赖更新的PR

## 最佳实践

### 代码提交
- 提交前运行本地检查
- 保持提交信息清晰明确
- 小步快跑，频繁提交

### Pull Request
- 确保所有CI检查通过
- 添加适当的描述和测试说明
- 及时响应代码审查意见

### 版本发布
- 遵循语义化版本规范
- 在CHANGELOG中记录变更
- 测试发布的二进制文件

## 故障排除

### CI失败常见原因
1. **代码格式**: 运行 `gofmt -w .` 修复
2. **Linting错误**: 根据golangci-lint输出修复
3. **测试失败**: 检查测试代码和逻辑
4. **依赖问题**: 运行 `go mod tidy` 整理

### 发布失败
1. **标签格式**: 确保使用 `v` 前缀（如 `v1.0.0`）
2. **权限问题**: 检查GitHub token权限
3. **构建错误**: 查看具体平台的构建日志

### Dependabot问题
1. **PR冲突**: 手动解决依赖冲突
2. **测试失败**: 更新代码以兼容新依赖
3. **配置错误**: 检查 `.github/dependabot.yml` 语法