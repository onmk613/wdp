#!/bin/bash

# wdp server 启动与管理脚本
#
# 用法:
#   ./scripts/server.sh start [build]   # 启动（二进制缺失自动构建；给 build 强制重建）
#   ./scripts/server.sh stop            # 停止
#   ./scripts/server.sh restart [build] # 重启
#   ./scripts/server.sh status          # 运行状态
#   ./scripts/server.sh log [N]         # 查看最近 N 行日志（缺省 50，follow 可加 -f 参数自取）
#
# 环境变量:
#   WDP_ADDR        监听地址（默认 127.0.0.1:7603）
#                   目标机纳管需要回连：WDP_ADDR=0.0.0.0:7603 + WDP_ADVERTISE=http://<外部IP>:7603
#   WDP_ADVERTISE   纳管脚本回连 server 的外部基址（见 wdp server --advertise）
#   WDP_DATA        数据目录（默认 ./wdp-data：SQLite + 纳管 CA + 日志都在这里）
#   WDP_ADMIN_USER  管理员用户名（默认 admin）
#   WDP_ADMIN_PASS  管理员密码：每次启动确保账号密码与之一致（改值后 restart 即改密）；
#                   缺省仅首启随机生成，打印在日志开头一次
#   NO_FRONTEND=1   构建时跳过前端（不内嵌控制台，访问 / 返回引导页，API 不受影响）
#
# 构建：需要时会编译全平台二进制到 bin/（linux/darwin × amd64/arm64、
# windows），server 按 agent 目标机架构取同级对应文件推装——bin/ 即
# agent 二进制目录，必须与运行中的 server 二进制一起保留。
#
# 示例:
#   ./scripts/server.sh start                 # 本机体验，浏览器开 http://127.0.0.1:7603
#   WDP_ADDR=0.0.0.0:7603 WDP_ADVERTISE=http://10.0.0.5:7603 ./scripts/server.sh start
#   WDP_ADMIN_PASS='xxx' ./scripts/server.sh restart build

set -euo pipefail
cd "$(dirname "$0")/.."

# 平台探测（不依赖 go：有二进制即可启动）
case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *)      OS=$(uname -s | tr '[:upper:]' '[:lower:]') ;;
esac
case "$(uname -m)" in
  x86_64)          ARCH=amd64 ;;
  aarch64|arm64)   ARCH=arm64 ;;
  *)               ARCH=$(uname -m) ;;
esac
BIN="bin/wdp-${OS}-${ARCH}"

DATA_DIR="${WDP_DATA:-wdp-data}"
PID_FILE="$DATA_DIR/wdp-server.pid"
LOG_FILE="$DATA_DIR/wdp-server.log"
ADDR="${WDP_ADDR:-127.0.0.1:7603}"

# server_pids 找出本地址的运行中 server pid 列表。pid 文件是首选（脚本
# 自己启动的精确记录），但它不是唯一事实来源：手动启动不写文件、start
# 失败自清理会删文件、文件被误删——都曾导致"实际在运行却报未在运行"
# （端口被占 → 后续 start 全部 bind 失败）。失效时兜底：lsof 按端口找
# 监听进程（同一端口仅一个监听者，无歧义）并核对命令行确是本二进制
# 的 server 子命令（不误停恰好占用端口的别的程序）；无 lsof 时退回
# pgrep 按命令行粗筛。agent 在目标机上运行，与本机进程无关。
server_pids() {
  if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    cat "$PID_FILE"
    return 0
  fi
  local port="${ADDR##*:}" pids="" pid
  if command -v lsof >/dev/null 2>&1; then
    for pid in $(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null); do
      if ps -p "$pid" -o command= 2>/dev/null | grep -q "$BIN server"; then
        pids="$pids$pid "
      fi
    done
    echo "${pids%% }"
    return 0
  fi
  pgrep -f "$BIN server .*--addr $ADDR" 2>/dev/null || true
}

is_running() {
  [ -n "$(server_pids)" ]
}

# 全平台构建：bin/ 是 agent 二进制目录（server 推装按目标机架构取同级文件），
# 只编当前平台会导致跨架构安装无二进制可用。
build() {
  local flags=()
  if [ "${NO_FRONTEND:-0}" != "1" ] && command -v npm >/dev/null 2>&1; then
    flags+=(--frontend)
  fi
  ./build.sh ${flags[@]+"${flags[@]}"}
}

start() {
  if is_running; then
    echo "wdp server 已在运行 (pid $(server_pids | tr '\n' ' ')): http://$(probe_addr)"
    exit 0
  fi
  # 二进制缺失自动构建；start build 强制重建
  if [ ! -x "$BIN" ] || [ "${1:-}" = "build" ]; then
    build
  fi
  # 数据目录与日志收敛权限：首启未显式配密码时 server 会把随机生成的
  # 管理员密码打印进日志（0644 的话本机任意用户可读），CA/DB 也在该目录
  mkdir -p "$DATA_DIR" && chmod 700 "$DATA_DIR"
  touch "$LOG_FILE" && chmod 600 "$LOG_FILE"

  local args=(server --addr "$ADDR" --data "$DATA_DIR")
  [ -n "${WDP_ADVERTISE:-}" ] && args+=(--advertise "$WDP_ADVERTISE")
  [ -n "${WDP_ADMIN_USER:-}" ] && args+=(--admin-user "$WDP_ADMIN_USER")
  [ -n "${WDP_ADMIN_PASS:-}" ] && args+=(--admin-pass "$WDP_ADMIN_PASS")

  nohup "$BIN" ${args[@]+"${args[@]}"} >>"$LOG_FILE" 2>&1 &
  echo $! >"$PID_FILE"

  # 就绪探测：/ 无需登录（200=已内嵌前端，503=未构建前端，均算就绪）
  local probe; probe="$(probe_addr)"
  if command -v curl >/dev/null 2>&1; then
    for _ in $(seq 1 50); do
      if ! is_running; then
        echo "启动失败（pid 已退出），日志尾部:" >&2
        tail -20 "$LOG_FILE" >&2 || true
        rm -f "$PID_FILE"
        exit 1
      fi
      if curl -s -o /dev/null --max-time 1 "http://$probe/"; then
        echo "wdp server 已启动: http://$probe (pid $(cat "$PID_FILE"))"
        echo "管理员账号: ${WDP_ADMIN_USER:-admin}"
        if [ -n "${WDP_ADMIN_PASS:-}" ]; then
          echo "管理员密码: 已按 WDP_ADMIN_PASS 生效（不回显，避免留在终端记录里）"
        else
          echo "管理员密码: 未配置——首启随机生成在日志开头；设 WDP_ADMIN_PASS 后 restart 可显式指定/重置"
        fi
        echo "agent 二进制目录: bin/ ($(ls bin/wdp-* 2>/dev/null | wc -l | tr -d ' ') 个平台: $(ls bin/wdp-* 2>/dev/null | sed 's|bin/wdp-||;s|\.exe$||' | tr '\n' ' '))"
        echo "日志: ${LOG_FILE}"
        exit 0
      fi
      sleep 0.2
    done
    echo "警告: 10 秒内未探到就绪信号，进程仍在运行，请查看日志: $LOG_FILE"
  else
    echo "wdp server 已启动 (pid $(cat "$PID_FILE"))（无 curl，跳过就绪探测）"
  fi
}

# probe_addr 把 0.0.0.0 归一为 127.0.0.1 供本机探测
probe_addr() {
  echo "${ADDR/0.0.0.0\//127.0.0.1/}" | sed 's|^0.0.0.0:|127.0.0.1:|'
}

# stop 只 return 不 exit：restart 的 `stop; start` 序列依赖继续执行。
# 逐个停掉 server_pids 找到的进程（兜底路径下 pid 文件可能缺失或指向
# 已死进程，rm -f 恒做清理）。
stop() {
  local pids; pids="$(server_pids)"
  if [ -z "$pids" ]; then
    echo "wdp server 未在运行"
    rm -f "$PID_FILE"
    return 0
  fi
  for pid in $pids; do
    kill "$pid" 2>/dev/null || true
  done
  for _ in $(seq 1 50); do
    local alive=""
    for pid in $pids; do
      kill -0 "$pid" 2>/dev/null && alive="$alive $pid"
    done
    if [ -z "$alive" ]; then
      echo "wdp server 已停止 (pid$(echo $pids | tr '\n' ' '))"
      rm -f "$PID_FILE"
      return 0
    fi
    sleep 0.2
  done
  echo "优雅停止超时，强制 kill (pid$(echo $pids | tr '\n' ' '))"
  for pid in $pids; do
    kill -9 "$pid" 2>/dev/null || true
  done
  rm -f "$PID_FILE"
}

status() {
  if is_running; then
    echo "运行中 (pid $(server_pids | tr '\n' ' ')): http://$(probe_addr) · 数据 $DATA_DIR · 日志 $LOG_FILE"
  else
    echo "未运行"
    exit 1
  fi
}

show_log() {
  [ -f "$LOG_FILE" ] || { echo "日志不存在: $LOG_FILE"; exit 1; }
  tail -n "${1:-50}" "$LOG_FILE"
}

case "${1:-}" in
  start)   shift; start "${1:-}" ;;
  stop)    stop ;;
  restart) shift; stop; start "${1:-}" ;;
  status)  status ;;
  log)     shift; show_log "${1:-50}" ;;
  *)
    sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
