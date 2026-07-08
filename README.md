# Tg-Down — Telegram 群聊媒体下载器

[![Release](https://img.shields.io/github/v/release/Heartcoolman/Tg-Down)](https://github.com/Heartcoolman/Tg-Down/releases)
[![CI](https://github.com/Heartcoolman/Tg-Down/actions/workflows/ci.yml/badge.svg)](https://github.com/Heartcoolman/Tg-Down/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

基于官方 TDLib 引擎的 Telegram 媒体下载工具：批量下载群组/频道历史媒体，实时监控新消息，
内置 Apple 风格 Web 管理台。单二进制运行，提供多架构 Docker 镜像与飞牛OS (fnOS) 部署模板。

## 功能特性

- 🌐 **Web 管理端**：网页内登录（验证码/两步验证）、聊天浏览、任务队列、下载历史、实时进度与日志
- 🚀 **官方 TDLib 引擎**：断点续传、CDN 加速、动态分片、DC 迁移全部原生处理
- 💪 **断点续跑**：进程重启后任务从扫描游标自动恢复并补下中断文件；失败任务指数退避自动重试
- 🎯 **内容级去重**：同一文件被转发到多个聊天只下载一次（按 TDLib unique_id 命中后本地复制）
- 🎛️ **任务级过滤器**：按媒体类型 / 日期区间 / 单文件大小过滤历史下载
- 🔗 **t.me 链接下载**：粘贴链接或 @用户名 直接下载，消息链接精确到单条消息
- ⏰ **定时下载**：按间隔自动增量扫描指定聊天
- 📣 **完成通知**：任务完成/失败可通知 Saved Messages 或 webhook
- 🖼️ **相册聚合与元数据**：相册归入 `album_<id>` 子目录；可选 `<文件>.json` 元数据 sidecar
- 🗂️ **任务队列与历史**：多任务排队、取消/重试；下载历史持久化，支持筛选/搜索/分页
- 📁 **分类存储**：按媒体类型归档（`disable_classify_by_type: true` 恢复扁平布局）

## 快速开始

先在 [my.telegram.org](https://my.telegram.org/apps) 创建应用，获取 `api_id` 与 `api_hash`
（也可以留空，首次在网页端登录时填写）。

### 方式一：Docker（推荐）

```bash
docker run -d --name tg-down \
  -p 8080:8080 \
  -e TG_DOWN_WEB_TOKEN=change-me \
  -e PUID=1000 -e PGID=1000 -e TZ=Asia/Shanghai \
  -v $PWD/downloads:/downloads -v $PWD/sessions:/sessions -v $PWD/data:/data \
  ghcr.io/heartcoolman/tg-down:latest
```

浏览器打开 `http://<主机>:8080?token=change-me`，网页内完成 Telegram 登录即可使用。

- 镜像为多架构（amd64 / arm64），纯环境变量配置，凭据可留空由网页端填写；
- `TG_DOWN_WEB_TOKEN` **必填**：绑定非回环地址时强制鉴权，缺失将拒绝启动；
- 数据卷：`/downloads`（下载文件）、`/sessions`（Telegram 会话）、`/data`（任务数据库）。

### 方式二：飞牛OS (fnOS)

1. 打开 fnOS「Docker」应用 → Compose → 新建项目；
2. 粘贴 [`docs/fnos/docker-compose.yml`](docs/fnos/docker-compose.yml) 模板，修改
   `TG_DOWN_WEB_TOKEN` 与卷映射路径（fnOS 存储路径形如 `/vol1/...`，文件管理器
   「复制原始路径」可获取；`PUID/PGID` 用 `id` 命令查看）；
3. 部署后访问 `http://<NAS地址>:8080?token=<令牌>` 完成网页登录；
4. 下载文件属主为 `PUID:PGID`，可直接经 SMB / 相册应用访问。

### 方式三：预编译包

从 [Releases](https://github.com/Heartcoolman/Tg-Down/releases) 下载对应平台压缩包：

| 平台 | 说明 |
|------|------|
| `tg-down-linux-amd64.tar.gz` | 自包含（OpenSSL 已捆绑于 `lib/`），glibc ≥ 2.29 即可运行（Debian 10+ / Ubuntu 19.04+） |
| `tg-down-darwin-arm64.tar.gz` | Apple Silicon；需 `brew install openssl@3` |

```bash
tar -xzf tg-down-linux-amd64.tar.gz && cd tg-down-linux-amd64
# 编辑 config.yaml 填入 API 信息（或使用环境变量）
./start.sh              # CLI 交互模式
./start.sh --web        # Web 管理端（127.0.0.1:8080）
```

> Windows 及其他交叉目标因 CGo 暂不提供预编译包，可按方式四自行构建。

### 方式四：源码构建

依赖：Go 1.25+、cmake、gperf、OpenSSL（macOS：`brew install cmake gperf openssl@3`；
Linux：`apt install cmake gperf libssl-dev zlib1g-dev g++`）。

```bash
git clone https://github.com/Heartcoolman/Tg-Down.git && cd Tg-Down
make tdlib    # 首次必需：构建安装 TDLib 到 ~/.tdlib，约 30-60 分钟
make build    # 编译（自动设置 CGo 环境与版本号）
cp config.yaml.example config.yaml   # 填入 API 信息
./tg-down
```

> 自定义 TDLib 安装位置：`TDLIB_PREFIX=/your/path make tdlib && make build`。

## 使用说明

### Web 管理端

```bash
./tg-down --web                 # 默认 127.0.0.1:8080
./tg-down --web :9000           # 自定义端口
./tg-down --web 0.0.0.0:8080    # 局域网访问（必须设置 TG_DOWN_WEB_TOKEN）
```

- **概览页**：选择聊天一键下载历史媒体 / 开启监控；粘贴 t.me 链接或 @用户名 解析下载
  （消息链接只下载该条消息）；「过滤器」面板设置媒体类型 / 日期区间 / 大小上限；
- **任务队列**：媒体级暂停/恢复、并发调节；批量任务取消/重试；**定时下载**计划管理
  （最小间隔 10 分钟，沿用过滤器设置，同聊天有任务在跑时自动跳过本次触发）；
- **下载历史**：按媒体类型 / 聊天 / 状态 / 时间筛选，支持搜索与分页；
- **设置页**：分类存储开关、媒体并发数、登出。

非回环监听时所有 API 需带令牌：`Authorization: Bearer <token>` 或 `?token=<token>`
（页面会自动记忆 URL 中的 token）。反向代理场景用 `TG_DOWN_WEB_ALLOWED_HOSTS`
配置允许的 Host（逗号分隔）。

### CLI 模式

直接运行 `./tg-down` 进入交互流程：首次输入验证码登录（会话自动持久化），选择聊天与模式
（1 下载历史 / 2 监控新消息 / 3 两者兼有）。其他命令：

```bash
./tg-down --version         # 显示版本
./tg-down --clear-session   # 清除会话，下次运行重新登录
```

### 任务完成通知

```yaml
notify:
  telegram_self: true                      # 发送到自己的 Saved Messages
  webhook_url: "https://example.com/hook"  # POST {"event":"task_finished","task":{...}}
```

任务完成或自动重试耗尽后的最终失败时触发（取消不通知），按任务粒度发送。

## 配置参考

配置优先级：环境变量 > `config.yaml` > 默认值。`config.yaml` 缺失时可纯环境变量运行。

| 配置项 | 环境变量 | 说明 | 默认值 |
|--------|----------|------|--------|
| `api.id` | `API_ID` | Telegram API ID | - |
| `api.hash` | `API_HASH` | Telegram API Hash | - |
| `api.phone` | `PHONE` | 手机号（国际格式） | - |
| `download.path` | `DOWNLOAD_PATH` | 下载根目录 | `./downloads` |
| `download.max_concurrent` | `MAX_CONCURRENT_DOWNLOADS` | 同时下载的文件数 | `5` |
| `download.batch_size` | `BATCH_SIZE` | 每批拉取的历史消息数 | `100` |
| `download.partition_size` | `PARTITION_SIZE` | 历史扫描在途媒体上限 | `100` |
| `download.save_metadata` | `SAVE_METADATA` | 写 `<文件>.json` 元数据 sidecar | `false` |
| `download.disable_classify_by_type` | - | 关闭按类型归档 | `false` |
| `queue.max_concurrent_tasks` | `MAX_CONCURRENT_TASKS` | 并行历史任务数（监控不占额） | `1` |
| `queue.auto_retry` | `AUTO_RETRY` | 任务失败自动重试次数（0 关闭） | `2` |
| `retry.max_retries` | `MAX_RETRIES` | 单文件网络重试次数 | `3` |
| `notify.telegram_self` | `NOTIFY_TELEGRAM_SELF` | 完成通知发 Saved Messages | `false` |
| `notify.webhook_url` | `NOTIFY_WEBHOOK_URL` | 完成通知 webhook 地址 | 空 |
| `store.path` | `STORE_PATH` | SQLite 数据库路径 | `./tg-down.db` |
| `session.dir` | `SESSION_DIR` | TDLib 会话根目录（位于 `<dir>/tdlib`） | `./sessions` |
| `chat.target_id` | `TARGET_CHAT_ID` | CLI 目标聊天 ID（0 = 交互选择） | `0` |
| `log.level` | `LOG_LEVEL` | debug / info / warn / error | `info` |

Web / 容器专用环境变量：

| 环境变量 | 说明 |
|----------|------|
| `TG_DOWN_WEB_TOKEN` | Web 访问令牌；绑定非回环地址时必填 |
| `TG_DOWN_WEB_ALLOWED_HOSTS` | 额外允许的 Host（反向代理域名，逗号分隔） |
| `TG_DOWN_NO_CONFIG_WRITE` | 非空时禁止写回 config.yaml（容器默认开启） |
| `PUID` / `PGID` / `TZ` | 容器内运行用户 / 组 / 时区 |

### 文件组织

```
downloads/
└── chat_123456789/           # 每聊天一个目录
    ├── photo/                # 按媒体类型归档（可关闭）
    │   ├── album_777/        # 同一相册归入子目录
    │   │   ├── photo_1.jpg
    │   │   └── photo_2.jpg
    │   └── photo_3.jpg
    └── video/
        ├── video_4.mp4
        └── video_4.mp4.json  # save_metadata 开启时的元数据 sidecar
```

## 开发

```
cmd/            入口（CLI / Web 模式、版本注入）
internal/
  config/       YAML + 环境变量配置
  telegram/     TDLib 客户端封装（认证 / 枚举 / 扫描 / 下载 / 监控 / 链接解析）
  downloader/   并发下载、暂停恢复、去重、路径规划、元数据
  queue/        任务队列、断点恢复、自动重试、定时调度
  store/        SQLite 持久化（任务 / 历史 / 定时计划，纯 Go 驱动）
  notify/       完成通知（Telegram / webhook）
  web/          Web 管理端（内嵌单页应用 + SSE）
  retry/        网络级重试
docker/         容器入口脚本
docs/fnos/      飞牛OS 部署模板
scripts/        TDLib 构建脚本
```

```bash
make build      # 编译（需先 make tdlib）
go test ./...   # 测试
golangci-lint run
```

CI/CD：push/PR 触发构建测试与 lint；发布由维护者手动打 `v*` tag 触发——原生构建
发布包（Linux 在 debian:11 容器内构建保证 glibc 兼容）+ GHCR 多架构镜像。
详见 [.github/WORKFLOWS.md](.github/WORKFLOWS.md)。

## 故障排除

| 问题 | 处理 |
|------|------|
| 认证失败 | 核对 `api_id` / `api_hash` / 手机号（国际格式，含 `+`） |
| Web 端 401 | URL 加 `?token=<TG_DOWN_WEB_TOKEN>` |
| 容器启动即退出 | 未设置 `TG_DOWN_WEB_TOKEN`（绑定 0.0.0.0 时必填） |
| Linux 报 GLIBC 版本错误 | 使用 v2.0.0+ 的发布包（glibc ≥ 2.29 即可）或 Docker 镜像 |
| macOS 报 openssl 缺失 | `brew install openssl@3` |
| 需要重新登录 | `./tg-down --clear-session` |
| TDLib 构建失败 | 确认 cmake/gperf/libssl-dev 已安装；内存 < 8GB 时 `JOBS=2 make tdlib` |

## 更新日志

### v2.0.0 (2026-07-08)

- 🔄 **可靠性**：任务断点续跑（重启后从扫描游标恢复、补下中断文件、监控自动恢复）；
  内容级去重（跨聊天同文件只下载一次）；任务失败自动重试（`queue.auto_retry`，指数退避）
- 🎯 **任务级过滤器**：媒体类型 / 日期区间 / 单文件大小上限
- 🔗 **t.me 链接下载**：链接 / @用户名 直接下载，消息链接精确到单条
- 🖼️ **相册聚合**（`album_<id>` 子目录）与 **元数据 sidecar**（`download.save_metadata`）
- ⏰ **定时下载** 与 📣 **完成通知**（Saved Messages / webhook）
- 🐳 **Docker 多架构镜像**（GHCR，amd64/arm64，PUID/PGID/纯环境变量配置）+ 飞牛OS 部署模板
- 🛠️ **Linux 发布包修复**：debian:11 构建（glibc ≥ 2.29 可运行，修复 #32），OpenSSL 捆绑
- 🔢 **版本注入**：`--version` / 启动横幅 / Web 页脚
- ⚠️ **破坏性变更**：移除配置 `download.chunk_size` / `download.max_workers` / `rate_limit.*`
  及对应环境变量（加载时警告）；移除 CLI `check` 命令与实验性 Rust helper；
  go-tdlib 升级至 TDLib 1.8.64（会话自动前向迁移，不支持回退）；SQLite schema 自动迁移
  （升级后不兼容 v1.x）；旧版相册文件在新路径下会重新下载一次

> 注：早期文档曾把 TDLib 迁移与任务队列标注为 "v2.0.0/v3.0.0"，但这两个版本号从未成为
> 实际 git tag——相关功能实际随 v1.4.0 发布，本条目为首个真实的 v2.0.0。

### v1.5.0 (2026-07-08)

- 🚿 扫描/下载流水线化（`partition_size` 控制在途上限）；⭐ 收藏夹（Saved Messages）支持；
  📊 进度口径与 SSE 限频修复；Web 管理台 v2（登出、中止登录、设置页）

### v1.4.0 (2026-07-05)

- 🔄 下载引擎切换为官方 TDLib（移除 gotd 与自研 session/floodwait/ratelimit 中间件）；
  🗂️ 任务队列与 💾 SQLite 持久化；🕘 下载历史记录；📁 按类型归档；🌐 Web 管理端首发；
  🧱 构建变更：首次需 `make tdlib`，发布改为原生构建

<details>
<summary>更早版本（v1.0.0 – v1.3.x）</summary>

- **v1.3.x (2025-07 ~ 2026-06)**：全自动发布流水线、开箱即用打包、智能版本管理、CI 修复
- **v1.2.0 (2025-07-29)**：代码质量与安全性改进、golangci-lint 全面接入
- **v1.1.0 (2025-07-27)**：并发下载优化、配置增强
- **v1.0.0 (2025-07-27)**：首个版本——历史下载、实时监控、并发下载、去重、会话持久化

</details>

## 许可证

MIT，详见 [LICENSE](LICENSE)。欢迎提交 Issue 和 Pull Request。
