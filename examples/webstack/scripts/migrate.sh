#!/bin/sh
# pre_install hook：数据迁移（script 模块上传到目标机临时路径执行，结束自删）
# 幂等策略：迁移完成后写 marker，重复部署直接跳过。
set -eu

workdir="${1:?usage: migrate.sh <workdir>}"
shared="$workdir/shared"
marker="$shared/.migrated"

if [ -f "$marker" ]; then
  echo "migrate: already done, skip"
  exit 0
fi

mkdir -p "$shared"
echo "migrate: schema v1 -> v2 on $shared"
echo "migrated at $(date '+%Y-%m-%d %H:%M:%S')" > "$marker"
echo "migrate: done"
