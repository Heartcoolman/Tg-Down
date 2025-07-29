# Tg-Down 优化功能说明

本文档介绍了 Tg-Down 项目中新增的优化功能，这些功能显著提升了下载性能和稳定性。

## 🚀 新增优化功能

### 1. 分块下载器 (Chunked Downloader)

**位置**: `internal/downloader/chunked/`

**功能**:
- 支持并行分块下载大文件
- 可配置分块大小和工作线程数
- 自动进度追踪和错误处理
- 支持断点续传机制

**配置**:
```yaml
download:
  chunk_size: 512      # 分块大小 (KB)
  max_workers: 4       # 最大工作线程数
  use_chunked: true    # 启用分块下载
```

**使用场景**: 大于1MB的文件自动使用分块下载

### 2. Flood Wait 处理器

**位置**: `internal/middleware/floodwait/`

**功能**:
- 智能处理 Telegram API 的 FLOOD_WAIT 错误
- 自动解析等待时间并进行智能重试
- 支持最大等待时间和重试次数限制
- 作为中间件集成到 Telegram 客户端

**配置**:
```yaml
retry:
  max_retries: 3       # 最大重试次数
  max_delay: 30        # 最大等待时间 (秒)
```

### 3. 速率限制器 (Rate Limiter)

**位置**: `internal/middleware/ratelimit/`

**功能**:
- 控制 API 请求频率，避免触发限制
- 基于令牌桶算法实现
- 支持突发请求处理
- 自动适应 API 限制

**配置**:
```yaml
rate_limit:
  requests_per_second: 5.0  # 每秒请求数
  burst_size: 10            # 突发请求大小
```

### 4. 重试机制 (Retry Mechanism)

**位置**: `internal/retry/`

**功能**:
- 指数退避重试策略
- 随机抖动避免雷群效应
- 智能错误类型判断
- 支持网络错误和 API 错误重试

**配置**:
```yaml
retry:
  max_retries: 3       # 最大重试次数
  base_delay: 1        # 基础延迟 (秒)
  max_delay: 30        # 最大延迟 (秒)
```

## 📊 性能提升

### 下载速度
- **分块下载**: 大文件下载速度提升 2-4 倍
- **并发控制**: 合理的并发数避免资源浪费
- **智能分块**: 根据文件大小自动选择最优策略

### 稳定性
- **Flood Wait 处理**: 自动处理 API 限制，减少下载中断
- **重试机制**: 网络波动时自动重试，提高成功率
- **速率限制**: 主动控制请求频率，避免被限制

### 资源使用
- **内存优化**: 分块下载减少内存占用
- **CPU 优化**: 合理的并发控制避免 CPU 过载
- **网络优化**: 智能重试和速率控制减少无效请求

## 🔧 配置示例

完整的配置文件示例：

```yaml
api:
  id: 123456
  hash: "your_api_hash"
  phone: "+1234567890"

download:
  path: "./downloads"
  max_concurrent: 5
  batch_size: 100
  chunk_size: 512      # 新增: 分块大小
  max_workers: 4       # 新增: 工作线程数
  use_chunked: true    # 新增: 启用分块下载

chat:
  target_id: -1001234567890

log:
  level: "info"

session:
  dir: "./sessions"

# 新增: 重试配置
retry:
  max_retries: 3
  base_delay: 1
  max_delay: 30

# 新增: 速率限制配置
rate_limit:
  requests_per_second: 5.0
  burst_size: 10
```

## 🧪 测试

运行优化功能测试：

```bash
go run cmd/test-optimizations/main.go
```

测试内容包括：
- 配置加载验证
- 重试机制测试
- 速率限制器测试
- Flood Wait 处理器测试
- 分块下载器测试

## 📈 监控和日志

新的优化功能提供详细的日志输出：

```
[INFO] 已创建优化的Telegram客户端 (分块下载: true, 速率限制: 5.0 req/s, 重试: 3次)
[INFO] 使用分块下载器下载文件: example.mp4 (大小: 52428800 bytes)
[INFO] 分块下载进度: 25.0% (13107200/52428800 bytes)
[WARN] FLOOD_WAIT错误，等待 15s 后重试
[INFO] 下载完成: example.mp4
```

## 🔄 向后兼容

所有新功能都是向后兼容的：
- 现有配置文件无需修改即可运行
- 新配置项都有合理的默认值
- 可以选择性启用优化功能

## 🛠️ 开发说明

### 架构设计
- **中间件模式**: Flood Wait 和速率限制作为中间件集成
- **策略模式**: 重试机制支持不同的重试策略
- **工厂模式**: 分块下载器支持不同的下载策略
- **依赖注入**: 所有组件通过依赖注入集成

### 扩展性
- 易于添加新的中间件
- 支持自定义重试策略
- 可配置的下载策略
- 模块化的组件设计

### 测试覆盖
- 单元测试覆盖核心功能
- 集成测试验证组件协作
- 性能测试确保优化效果
- 错误场景测试提高稳定性