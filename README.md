<div align="center">

# 📥 Tg-Down

**基于官方 TDLib 引擎的 Telegram 群聊媒体下载器**

批量下载群组/频道历史媒体 · 实时监控新消息 · 内置 Web 管理台

[![Release](https://img.shields.io/github/v/release/Heartcoolman/Tg-Down)](https://github.com/Heartcoolman/Tg-Down/releases)
[![CI](https://github.com/Heartcoolman/Tg-Down/actions/workflows/ci.yml/badge.svg)](https://github.com/Heartcoolman/Tg-Down/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)
![TDLib](https://img.shields.io/badge/TDLib-1.8.64-2CA5E0?logo=telegram&logoColor=white)
![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

[功能特性](#-功能特性) · [快速开始](#-快速开始) · [使用说明](#-使用说明) · [配置参考](#-配置参考) · [故障排除](#-故障排除) · [更新日志](#-更新日志)

单二进制运行 · 多架构 Docker 镜像 · 飞牛OS (fnOS) 部署模板

</div>

---

## ✨ 功能特性

**引擎与可靠性**

- 🚀 **官方 TDLib 引擎**：断点续传、CDN 加速、动态分片、DC 迁移全部原生处理
- 💪 **断点续跑**：进程重启后任务从扫描游标自动恢复并补下中断文件；失败任务指数退避自动重试
- 🎯 **内容级去重**：同一文件被转发到多个聊天只下载一次（按 TDLib `unique_id` 命中后本地复制）

**下载控制**

- 🎛️ **任务级过滤器**：按媒体类型 / 日期区间 / 单文件大小过滤历史下载
- 🔗 **t.me 链接下载**：粘贴链接或 @用户名 直接下载，消息链接精确到单条消息
- ⏰ **定时下载**：按间隔自动增量扫描指定聊天（最小间隔 10 分钟）
- 🖼️ **相册聚合与元数据**：相册归入 `album_<id>` 子目录；可选 `<文件>.json` 元数据 sidecar
- 🗂️ **分类存储与路径模板**：按媒体类型归档，落盘布局可自定义模板

**Web 管理台**

- 🌐 **网页内完成一切**：登录（验证码/两步验证）、聊天浏览、任务队列、下载历史、实时进度与日志
- 🧩 **媒体画廊**：缩略图预览、相册分组、单个媒体暂停/恢复，历史文件在线查看
- ⏱️ **时间线**：TG 风格本地浏览已下载内容——频道列表、日期分组消息流、多标签筛选（交集/并集）、最新在底上翻看旧
- 💬 **聊天导出**：HTML 归档（类似 Telegram Desktop export）

**通知与部署**

- 📣 **完成通知**：任务完成/失败推送到 Saved Messages 或 webhook
- 🖥️ **桌面客户端**：Windows / macOS / Linux 桌面应用，内嵌引擎 + 远程实例统一管理（见 [docs/desktop.md](docs/desktop.md)）
- 🐳 **多种部署方式**：单二进制、多架构 Docker 镜像（amd64/arm64）、飞牛OS Compose 模板、Linux/macOS/Windows 预编译包

---

## 🚀 快速开始

> [!NOTE]
> 先在 [my.telegram.org](https://my.telegram.org/apps) 创建应用获取 `api_id` 与 `api_hash`；
> 也可以留空，首次在网页端登录时填写。

### Docker（推荐）

```bash
TOKEN="$(openssl rand -hex 32)"
docker run -d --name tg-down \
  -p 8080:8080 \
  -e TG_DOWN_WEB_TOKEN="$TOKEN" \
  -e PUID=1000 -e PGID=1000 -e TZ=Asia/Shanghai \
  -v $PWD/downloads:/downloads -v $PWD/sessions:/sessions -v $PWD/data:/data \
  ghcr.io/heartcoolman/tg-down:latest
```

浏览器打开 `http://<主机>:8080?token=<上面生成的 TOKEN>`，网页内完成 Telegram 登录即可使用。

> [!IMPORTANT]
> `TG_DOWN_WEB_TOKEN` 必填（至少 16 个字符）：绑定非回环地址时强制鉴权。

> [!TIP]
> 主机无法直连 Telegram 时追加 `-e TG_PROXY="socks5://192.168.1.10:7890"`（或 `http://...`），
> 否则会一直停在「正在连接 Telegram」。

要点：

- 镜像为多架构（amd64 / arm64），凭据可留空由网页端填写；
- 网页凭据以 0600 权限保存在 `/data/config.yaml`，保留 `/data` 卷即可在重建容器后免重新填写；
- 数据卷：`/downloads`（下载文件）、`/sessions`（Telegram 会话）、`/data`（任务数据库和配置）。

### 飞牛OS (fnOS)

1. 打开 fnOS「Docker」应用 → Compose → 新建项目；
2. 粘贴 [`docs/fnos/docker-compose.yml`](docs/fnos/docker-compose.yml) 模板，修改 `TG_DOWN_WEB_TOKEN`
   与卷映射路径（fnOS 存储路径形如 `/vol1/...`，文件管理器「复制原始路径」可获取；`PUID/PGID` 用 `id` 命令查看）；
3. 部署后访问 `http://<NAS地址>:8080?token=<令牌>` 完成网页登录；
4. 下载文件属主为 `PUID:PGID`，可直接经 SMB / 相册应用访问。

### 桌面客户端

不想用命令行或 Docker？桌面客户端（Windows / macOS / Linux）内嵌完整引擎，安装即用，
并支持把多台 Docker 部署接入同一窗口统一管理。下载与构建见 **[docs/desktop.md](docs/desktop.md)**。

### 预编译包

从 [Releases](https://github.com/Heartcoolman/Tg-Down/releases) 下载对应平台压缩包：| 平台 | 包名 | 说明 |
|------|------|------|
| Linux x86_64 | `tg-down-linux-amd64.tar.gz` | 自包含（OpenSSL 已捆绑于 `lib/`），glibc ≥ 2.31 即可运行（Debian 11+ / Ubuntu 20.04+） |
| macOS Apple Silicon | `tg-down-darwin-arm64.tar.gz` | 需 `brew install openssl@3` |
| Windows x86_64 | `tg-down-windows-amd64.zip` | MinGW-w64 构建，解压即用 |

```bash
tar -xzf tg-down-linux-amd64.tar.gz && cd tg-down-linux-amd64
# 编辑 config.yaml 填入 API 信息（或使用环境变量）
./start.sh              # CLI 交互模式
./start.sh --web        # Web 管理端（127.0.0.1:8080）
```

### 源码构建

依赖：Go 1.25+、cmake、gperf、OpenSSL（macOS：`brew install cmake gperf openssl@3`；
Linux：`apt install cmake gperf libssl-dev zlib1g-dev g++`）。

```bash
git clone https://github.com/Heartcoolman/Tg-Down.git && cd Tg-Down
make tdlib    # 首次必需：构建安装 TDLib 到 ~/.tdlib，约 30-60 分钟
make build    # 编译（自动设置 CGo 环境与版本号）
install -m 600 config.yaml.example config.yaml   # 填入 API 信息
./tg-down
```

自定义 TDLib 安装位置：`TDLIB_PREFIX=/your/path make tdlib && make build`。

---

## 💡 使用说明

### Web 管理端

```bash
./tg-down --web                 # 默认 127.0.0.1:8080
./tg-down --web :9000           # 自定义端口
./tg-down --web 0.0.0.0:8080    # 局域网访问（必须设置 TG_DOWN_WEB_TOKEN）
```

| 页面 | 能力 |
|------|------|
| 概览 | 选择聊天一键下载历史媒体 / 开启监控；粘贴 t.me 链接或 @用户名 解析下载（消息链接只下载该条消息）；「过滤器」面板设置媒体类型 / 日期区间 / 大小上限 |
| 任务队列 | 媒体级暂停/恢复、并发调节；批量任务取消/重试；定时下载计划管理（同聊天有任务在跑时自动跳过本次触发） |
| 画廊 | 缩略图预览、相册分组、单个媒体暂停/恢复、历史文件在线查看 |
| 时间线 | 按频道浏览已下载内容：TG 风格消息流（日期分组 / 相册 / 频道与群组形态区分），消息倒序（最新在底、上翻加载更早），点击消息里 `#标签` 可叠加筛选（并集/交集切换），外观面板可调背景 / 透明度 / 毛玻璃 |
| 历史 | 按媒体类型 / 聊天 / 状态 / 时间筛选，支持搜索与分页 |
| 设置 | 分类存储开关、媒体并发数、登出 |

鉴权规则：

- 非回环监听时所有 API 需带令牌：`Authorization: Bearer <token>` 或 `?token=<token>`
  （页面会自动把 URL 中的 token 换成 HttpOnly Cookie）；
- 反向代理场景用 `TG_DOWN_WEB_ALLOWED_HOSTS` 配置允许的 Host（逗号分隔），并设置
  `TG_DOWN_WEB_TRUST_PROXY=1` 才会信任代理传入的 HTTPS 协议；
- 启用代理 Host 后，即使后端只监听 `127.0.0.1` 也必须设置令牌。只有代理能访问后端端口时才能开启
  `TG_DOWN_WEB_TRUST_PROXY`；若代理把 Host 改写成回环地址，程序无法自动判断公网入口，仍须主动设置令牌。

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

---

## 🧰 配置参考

配置优先级：环境变量 > `config.yaml` > 默认值。`config.yaml` 缺失时可纯环境变量运行。

<details open>
<summary><b>主配置项</b></summary>

| 配置项 | 环境变量 | 说明 | 默认值 |
|--------|----------|------|--------|
| `api.id` | `API_ID` | Telegram API ID | - |
| `api.hash` | `API_HASH` | Telegram API Hash | - |
| `api.phone` | `PHONE` | 手机号（国际格式） | - |
| `telegram.proxy` | `TG_PROXY` | Telegram 连接代理（见[代理](#代理)）；留空依次回退 `ALL_PROXY` / `HTTPS_PROXY` / `HTTP_PROXY`；`direct`/`off`/`none` 强制直连 | 空（直连） |
| `download.path` | `DOWNLOAD_PATH` | 下载根目录 | `./downloads` |
| `download.max_concurrent` | `MAX_CONCURRENT_DOWNLOADS` | 同时下载的文件数 | `5` |
| `download.batch_size` | `BATCH_SIZE` | 每批拉取的历史消息数 | `100` |
| `download.partition_size` | `PARTITION_SIZE` | 历史扫描在途媒体上限 | `100` |
| `download.save_metadata` | `SAVE_METADATA` | 写 `<文件>.json` 元数据 sidecar | `false` |
| `download.disable_classify_by_type` | - | 关闭按类型归档 | `false` |
| `download.path_template` | - | 落盘路径模板（见[路径模板](#路径模板)） | `chat_{chat_id}/{type}/{album}/{name}` |
| `queue.max_concurrent_tasks` | `MAX_CONCURRENT_TASKS` | 并行历史任务数（监控不占额） | `1` |
| `queue.auto_retry` | `AUTO_RETRY` | 任务失败自动重试次数（0 关闭） | `2` |
| `retry.max_retries` | `MAX_RETRIES` | 单文件网络重试次数 | `3` |
| `retry.base_delay` | `BASE_DELAY` | 重试基础延迟（秒） | `1` |
| `retry.max_delay` | `MAX_DELAY` | 重试最大延迟（秒） | `30` |
| `notify.telegram_self` | `NOTIFY_TELEGRAM_SELF` | 完成通知发 Saved Messages | `false` |
| `notify.webhook_url` | `NOTIFY_WEBHOOK_URL` | 完成通知 webhook 地址 | 空 |
| `store.path` | `STORE_PATH` | SQLite 数据库路径 | `./tg-down.db` |
| `session.dir` | `SESSION_DIR` | TDLib 会话根目录（位于 `<dir>/tdlib`） | `./sessions` |
| `chat.target_id` | `TARGET_CHAT_ID` | CLI 目标聊天 ID（0 = 交互选择） | `0` |
| `log.level` | `LOG_LEVEL` | debug / info / warn / error | `info` |

</details>

<details>
<summary><b>Web / 容器专用环境变量</b></summary>

| 环境变量 | 说明 |
|----------|------|
| `TG_DOWN_WEB_TOKEN` | Web 访问令牌；非回环监听或配置代理 Host 时必填，至少 16 个字符 |
| `TG_DOWN_WEB_ALLOWED_HOSTS` | 额外允许的 Host（反向代理域名，逗号分隔） |
| `TG_DOWN_WEB_TRUST_PROXY` | `1` 时信任 `X-Forwarded-Proto`；仅用于无法绕过的可信代理 |
| `TG_DOWN_NO_CONFIG_WRITE` | 非空时禁止写回 config.yaml（只适合完全由环境变量管理凭据的部署） |
| `PUID` / `PGID` / `TZ` | 容器内运行用户 / 组 / 时区 |

</details>

### 代理

> [!WARNING]
> TDLib 不读 `HTTP_PROXY`/`HTTPS_PROXY` 等环境变量。服务器所在网络无法直连 Telegram 时不配代理，
> 会一直卡在「正在连接 Telegram」（30 秒后日志会提示）。

```yaml
# config.yaml
telegram:
  proxy: "socks5://127.0.0.1:1080"   # 带认证: socks5://user:pass@host:port
```

或用环境变量（容器部署更方便）：`TG_PROXY`，或通用的 `ALL_PROXY` / `HTTPS_PROXY` / `HTTP_PROXY`
（按此顺序取第一个非空值）。支持三种形式：

- `socks5://[user:pass@]host:port`（`socks`、`socks5h` 同义）
- `http://[user:pass@]host:port`：HTTP CONNECT 透明转发（Clash/V2Ray 等混合端口即可）
- `mtproto://secret@host:port`

特殊值 `direct` / `off` / `none` 表示强制直连：当环境里已存在通用代理变量、但希望 Telegram 流量直连时使用。

### 文件组织

默认布局（等价于模板 `chat_{chat_id}/{type}/{album}/{name}`）：

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

sidecar 内含该消息的完整信息：文案（caption）、消息 ID、可读日期（`date_text`）、
聊天 ID/标题、发送者 ID、相册 ID、媒体类型/文件名/大小/MIME、文件唯一 ID（`unique_id`）、
任务 ID，以及可一键跳回原消息的 `message_url`（`https://t.me/c/<聊天>/<消息>`）。

### 路径模板

`download.path_template` 可自定义落盘路径。默认值即上方既有布局，不改模板则升级后文件位置不变。

| 占位符 | 展开为 |
|--------|--------|
| `{chat_id}` | 聊天数字 ID |
| `{chat_title}` | 聊天标题（缺失时退回 `chat_<id>`） |
| `{type}` | 媒体类型目录；关闭分类归档时该层级消失 |
| `{album}` | `album_<id>`；非相册消息时该层级消失 |
| `{date}` | 消息日期 `2024-03-05` |
| `{msg_id}` | 消息 ID |
| `{sender}` | 发送者 ID（未知为 `unknown`） |
| `{name}` | 文件名（含扩展名，已带消息 ID 前缀） |
| `{ext}` | 扩展名（含点） |

展开为空的路径段会被丢弃，因此 `{type}` / `{album}` 能自然地「消失」。

> [!CAUTION]
> 模板必须至少包含 `{chat_id}` 与 `{chat_title}` 之一（聊天隔离层），并至少包含 `{name}` 或 `{msg_id}`：
> `{chat_id}` 或 `{chat_title}` 隔离不同聊天（`{chat_title}` 在标题缺失时自动回退为 `chat_<id>`），
> `{name}` / `{msg_id}` 隔离同一聊天中的不同消息。缺少任一层都会让文件互相覆盖，或被当成「已下载」而跳过。
> 非法模板会被忽略并回退到默认布局。

```yaml
download:
  # 按 ID、标题和日期归档：downloads/123456/我的频道/2024-03-05/42_video.mp4
  path_template: "{chat_id}/{chat_title}/{date}/{name}"
```

不想看到 `chat_<id>` 这种目录名？让顶层目录直接用群聊名即可（标题缺失时仍会回退 `chat_<id>` 兜底）：

```yaml
download:
  # 顶层目录 = 群聊名：downloads/我的频道/2024-03-05/42_video.mp4
  path_template: "{chat_title}/{date}/{name}"
```

---

## 🧭 项目结构

```
cmd/            入口（CLI / Web 模式、版本注入）
internal/
  config/       YAML + 环境变量配置
  telegram/     TDLib 客户端封装（认证 / 枚举 / 扫描 / 下载 / 监控 / 链接解析）
  downloader/   并发下载、暂停恢复、去重、路径规划、元数据
  queue/        任务队列、断点恢复、自动重试、定时调度
  store/        SQLite 持久化（任务 / 历史 / 定时计划，纯 Go 驱动）
  notify/       完成通知（Telegram / webhook）
  web/          Web 管理端（内嵌 Vite + Preact 单页应用 + SSE）
  retry/        网络级重试
web/            前端源码（构建产物内嵌进二进制）
docker/         容器入口脚本
docs/fnos/      飞牛OS 部署模板
scripts/        TDLib 构建脚本
```

```bash
make build      # 编译（需先 make tdlib）
npm ci && npm run build   # 前端依赖安装与构建（make build 会自动执行）
go test ./...   # 测试
golangci-lint run
```

CI/CD：push/PR 触发构建测试与 lint；发布由维护者手动打 `v*` tag 触发，原生构建
发布包（Linux 在 debian:11 容器内构建保证 glibc 兼容）并推送 GHCR 多架构镜像。
详见 [.github/WORKFLOWS.md](.github/WORKFLOWS.md)。

---

## 🩺 故障排除

| 问题 | 处理 |
|------|------|
| 认证失败 | 核对 `api_id` / `api_hash` / 手机号（国际格式，含 `+`） |
| 一直卡在「正在连接 Telegram」 | 网络无法直连 Telegram，配置代理即可：`telegram.proxy` 或环境变量 `TG_PROXY` / `ALL_PROXY` / `HTTPS_PROXY` / `HTTP_PROXY`（详见[代理](#代理)） |
| Web 端 401 | URL 加 `?token=<TG_DOWN_WEB_TOKEN>` |
| 容器启动即退出 | 未设置 `TG_DOWN_WEB_TOKEN`（绑定 0.0.0.0 时必填） |
| Linux 报 GLIBC 版本错误 | 使用发布包（glibc ≥ 2.31 即可）或 Docker 镜像 |
| macOS 报 openssl 缺失 | `brew install openssl@3` |
| 需要重新登录 | `./tg-down --clear-session` |
| TDLib 构建失败 | 确认 cmake/gperf/libssl-dev 已安装；内存 < 8GB 时 `JOBS=2 make tdlib` |

---

## 📜 更新日志

### v3.3.0 (2026-08-26)

时间线本地浏览：像 Telegram 一样翻看过往下载。

- ⏱️ **时间线视窗**：web 管理台新增「时间线」页——扫描下载目录的 sidecar 元数据建立索引
  （30s TTL 自动刷新，也可手动强制重建），按频道浏览已下载内容
- 📄 **TG 风格消息流**：日期分组、相册网格、发送者信息；频道与群组按 TDLib 类型区分形态
  （频道靠左气泡无头像，群组带头像与发送者名）
- 🏷️ **多标签筛选**：消息里的 `#标签` 可点击叠加，支持**并集/交集**切换；标签匹配按词边界
  处理（`#马赛克X` 不误配 `#马赛克`），中文与英文标签均可用
- 🔄 **消息倒序展示**：最新消息在最底部，向上滚动加载更早（分页游标加载，视口不跳动）
- 🎨 **外观自定义**：4 套内置背景 + 自定义图片 URL、面板/气泡透明度、毛玻璃模糊（localStorage 持久化）
- 🗑️ **历史记录管理**：历史页与任务卡片可删除/清空下载记录（只删数据库记录，磁盘文件保留）
- 🔐 标签点击走事件委托，兼容管理台 CSP（`script-src 'self'`，行内 handler 会被浏览器拦截）
- 📚 sidecar 元数据补全 `chat_title` / `date_text` / `message_url` / `unique_id` / `task_id` 字段

### v3.2.0 (2026-08-24)

收敛版本，不加新功能：修复 v3.0/v3.1 的交付缺陷，补齐规划遗留项。

- 🩹 **桌面端发布包修复**：v3.1 的三个桌面发布作业漏掉了前端构建步骤，`go:embed` 嵌进的是
  空目录，**所有 v3.1 桌面包打开后都是"前端尚未构建"占位页**；同时 Windows 安装包
  （`-setup.exe`）因 CI 未装 NSIS 从未真正产出、rpm 沿用 Debian 包名导致依赖无法满足。
  **建议 v3.1 桌面端用户直接升级到本版本**
- 🎯 **默认下载类型不再包含贴纸**：贴纸没有服务端检索能力，此前把它放进默认集，使
  "不指定类型"的任务整体退化为遍历完整历史且进度条失去分母——而这正是最常见的建任务方式。
  需要贴纸请显式勾选（会提示完整扫描的代价）
- ⚡ **历史页性能**：分页改游标（深翻页从 4.8ms 降到 0.16ms，与首页持平）、文件名搜索改
  FTS5 全文索引、统计结果缓存、列表不再搬运缩略图二进制；翻页不再重复统计总数
- 🔒 **桌面端安全边界**：内嵌界面补齐 CSP 等安全响应头，壳层补同源校验（此前反代为让引擎
  放行而剥离 Origin，使引擎的 CSRF 防护对经壳请求失效）
- 🐞 **桌面端功能修复**：新增远程实例时访问令牌被静默丢弃（实例随后一直 401）；单实例锁
  提前到打开数据库之前（此前第二个进程已开库并起了 TDLib）；自启路径含空格即失效；
  端口被占用等错误此前只表现为界面一直"正在连接"
- 📊 **任务卡片显示速度与剩余时间**；任务卡片可下钻查看该任务下载的文件；历史页补排序与
  CSV/JSON 导出；落盘路径模板可在设置页修改
- 🖼️ 画廊：动画贴纸（.tgs/.webm）不再裂图，按载体分别渲染
- 📁 内容去重复制出的文件补写元数据 sidecar（此前同一份内容先在哪个聊天下载，决定了另一处
  有没有 `.json`）
- 🧪 **质量基线**：测试代码纳入 lint（此前约 8600 行被整体豁免）；新增前端测试车道与 macOS
  CI 作业；补 6 个真实账号集成测试场景
- ⚠️ **破坏性变更**：SQLite schema 升至 v4（自动迁移，升级后不可用 v3.1 及更早版本打开）；
  `/api/history` 分页参数由 `page/page_size` 改为游标 `cursor/limit`（`page_size` 仍兼容）

### v3.1.0 (2026-08-23)

- 🖥️ **桌面客户端**：新增 Windows / macOS / Linux 三平台桌面应用（Wails 壳），内嵌完整下载引擎
  （TDLib + 任务队列 + Web 管理台），安装即用；关闭即隐藏到托盘、开机自启、单实例锁、任务失败
  系统通知、启动时检查更新（详见 [docs/desktop.md](docs/desktop.md)）
- 🔗 **多实例管理**：桌面端可添加多台远程 Docker 实例（地址 + 访问令牌），同一窗口切换统一管理；
  凭据仅存本机 `instances.json`（0600），经壳层反代注入，不暴露给界面层
- ⚙️ **设置热更新**：设置页覆盖代理、并发、重试、元数据 sidecar、通知、日志级别等完整配置项，
  可热应用的项立即生效并写回 `config.yaml`
- 📜 **文件日志**：`log.file` / `LOG_FILE` 将日志落盘（10MB 轮转 ×3，桌面端默认开启）；
  日志页支持按级别过滤与错误/警告快捷视图
- 🧱 **引擎可嵌入**：`web.Server` 拆出 `Serve`（监听器注入）与 `UIHandler`（内嵌前端独立挂载），
  供桌面壳等宿主程序复用同一套装配链

### v3.0.0 (2026-08-22)

- 🔌 **连接代理**：新增 `TG_PROXY` 与 `telegram.proxy`（socks5 / http CONNECT / mtproto），
  未显式配置时自动回退通用代理变量；连接停滞 30s 循环输出排查提示，非法配置启动即报错（#49）
- 🖼️ **媒体画廊**：缩略图预览、相册分组、单个媒体暂停/恢复，历史文件在线查看
- 🧩 **管理台重构**：Vite + Preact 重写前端（任务/画廊/历史/定时/日志/设置分页），补齐安全响应头
- 📝 **路径模板**：`download.path_template` 自定义落盘布局（默认保持 v2 布局，老文件不会重下）
- 🎗️ **新媒体类型**：sticker、video note；统一媒体类型注册表
- 💬 **聊天导出**：HTML 归档（类似 Telegram Desktop export）
- 🪟 **Windows 支持**：新增 windows-amd64 发布包（MinGW-w64 构建）
- 🛡️ **可靠性**：单文件失败不再被静默吞掉（任务新增 `partial` 终态）、FLOOD_WAIT 按 `retry_after`
  精确退避等修复；SQLite schema 自动迁移至 v3
- ⚠️ **破坏性变更**：前端构建引入 Node/npm（贡献者需先 `npm ci`）；SQLite 库升级后不兼容 v2.x 之前

<details>
<summary><b>v2.0.0 及更早版本</b></summary>

#### v2.0.0 (2026-07-08)

- 🔄 任务断点续跑、内容级去重、任务失败自动重试（指数退避）
- 🎯 任务级过滤器；🔗 t.me 链接下载；🖼️ 相册聚合与元数据 sidecar
- ⏰ 定时下载；📣 完成通知（Saved Messages / webhook）
- 🐳 Docker 多架构镜像（PUID/PGID、持久化网页凭据）+ 飞牛OS 部署模板
- 🛠️ Linux 发布包改为 debian:11 构建（glibc ≥ 2.31，修复 #32），OpenSSL 捆绑
- ⚠️ 移除 `download.chunk_size` / `download.max_workers` / `rate_limit.*`；
  go-tdlib 升级至 TDLib 1.8.64（会话前向迁移）；SQLite schema 自动迁移

#### v1.5.0 (2026-07-08)

- 扫描/下载流水线化（`partition_size`）；⭐ 收藏夹支持；Web 管理台 v2

#### v1.4.0 (2026-07-05)

- 下载引擎切换为官方 TDLib；任务队列 + SQLite 持久化 + 下载历史；Web 管理端首发

#### v1.0.0 – v1.3.x (2025-07 ~ 2026-06)

- 首个版本（历史下载、实时监控、并发下载、去重、会话持久化）及打包/CI 改进

</details>

---

<div align="center">

📄 MIT License · 详见 [LICENSE](LICENSE) · 欢迎提交 Issue 和 Pull Request

</div>
