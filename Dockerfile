# syntax=docker/dockerfile:1
# 多阶段构建：builder 编译 TDLib 与 Go 二进制，runtime 仅带运行时依赖。
# TDLib 层只依赖 scripts/install-tdlib.sh，源码变更不会触发耗时的 TDLib 重建。

FROM golang:1.25-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
      cmake gperf g++ make git zlib1g-dev libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# TDLib 层（可缓存，约 30-60 分钟）
COPY scripts/install-tdlib.sh /tmp/install-tdlib.sh
RUN TDLIB_PREFIX=/opt/tdlib TDLIB_SRC=/tmp/tdlib-src JOBS=$(nproc) \
      bash /tmp/install-tdlib.sh \
    && rm -rf /tmp/tdlib-src

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# linux 下 go-tdlib 绑定默认静态链接（tdjson_static），但其列表缺 tde2e 且 -L 指向
# /usr/local/lib，这里统一补全
ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/tdlib/include" \
    CGO_LDFLAGS="-L/opt/tdlib/lib -ltdjson_static -ltdjson_private -ltdclient -ltdcore -ltde2e -ltdmtproto -ltdactor -ltdapi -ltddb -ltdsqlite -ltdnet -ltdutils -lstdc++ -lssl -lcrypto -ldl -lz -lm"
RUN go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/tg-down ./cmd

FROM debian:12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      libssl3 zlib1g ca-certificates curl gosu tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/tg-down /usr/local/bin/tg-down
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 纯环境变量运行：无 config.yaml，凭据经 API_ID/API_HASH（或网页登录）注入
ENV STORE_PATH=/data/tg-down.db \
    SESSION_DIR=/sessions \
    DOWNLOAD_PATH=/downloads \
    TG_DOWN_NO_CONFIG_WRITE=1 \
    PUID=1000 \
    PGID=1000

VOLUME ["/downloads", "/sessions", "/data"]
EXPOSE 8080

# 非回环监听强制 TG_DOWN_WEB_TOKEN（缺失时进程拒绝启动），健康检查带同一 token
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD curl -fsS "http://127.0.0.1:8080/api/state?token=${TG_DOWN_WEB_TOKEN}" || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["--web", "0.0.0.0:8080"]
