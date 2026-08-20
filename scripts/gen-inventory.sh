#!/bin/sh
# 生成压测用 inventory：N 台本机主机（conn: local，不产生真实网络流量，
# 度量 wdp 编排引擎本身的并发/开销）。
#
# 用法：
#   ./scripts/gen-inventory.sh 1000 > /tmp/bench-inv.yaml
#   time ./bin/wdp run examples/bench/bench.yaml -i /tmp/bench-inv.yaml --forks 200
#
# 目标机视角的压测请把 conn: local 换成 ssh/agent 主机。
set -eu

N=${1:-100}
case "$N" in
  ''|*[!0-9]*) echo "用法: $0 <主机数>" >&2; exit 2 ;;
esac

echo "bench:"
echo "  hosts:"
i=1
while [ "$i" -le "$N" ]; do
  printf '    bench%05d: {conn: local}\n' "$i"
  i=$((i + 1))
done
