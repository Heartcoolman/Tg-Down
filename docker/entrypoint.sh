#!/bin/bash
# 容器入口：按 PUID/PGID 修正数据目录属主后降权运行 tg-down。
# 仅在顶层属主不符时 chown 目录本身（不递归），避免海量 downloads 拖慢启动；
# 首次启动（标记文件缺失）时对 /sessions /data 做一次递归修正。
set -euo pipefail

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

if ! getent group "$PGID" >/dev/null 2>&1; then
  groupadd -g "$PGID" tgdown
fi
if ! getent passwd "$PUID" >/dev/null 2>&1; then
  useradd -u "$PUID" -g "$PGID" -M -s /usr/sbin/nologin tgdown
fi

for dir in /downloads /sessions /data; do
  mkdir -p "$dir"
  if [ "$(stat -c '%u:%g' "$dir")" != "$PUID:$PGID" ]; then
    chown "$PUID:$PGID" "$dir"
  fi
done

marker=/data/.ownership-initialized
if [ ! -f "$marker" ]; then
  chown -R "$PUID:$PGID" /sessions /data || true
  gosu "$PUID:$PGID" touch "$marker"
fi

exec gosu "$PUID:$PGID" tg-down "$@"
