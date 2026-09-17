#!/bin/bash

# wdp 构建脚本：缺省交叉编译全平台二进制，产出一份多架构 bin 目录
#
# 用法:
#   ./build.sh [os/arch …]
#
#   目标缺省 = 全平台（ALL_TARGETS）；关键字 all 同样展开全集；显式给出
#   目标时只编译指定平台。产物一律命名 bin/wdp-<os>-<arch>（windows 另带
#   .exe 后缀），bin/SHA256SUMS 记录校验和。
#   全平台集合：linux/amd64 linux/arm64 darwin/arm64 windows/amd64
#
#   多架构 bin 目录是控制端的分发单元：运行的二进制与其他平台二进制互为
#   同级文件，push 自举与 agentctl install 按目标机 uname 自动取同级对应
#   二进制上传——跨平台零配置，什么环境自动运行什么二进制。
#
#   -h, --help    本帮助
#
# 示例:
#   ./build.sh                          # 全平台 → bin/wdp-<os>-<arch>[.exe] + SHA256SUMS
#   ./build.sh all                      # 等价缺省
#   ./build.sh linux/amd64 linux/arm64  # 只编译指定平台

set -euo pipefail

cd "$(dirname "$0")"
mkdir -p bin

# print usage text from this script（头部注释块是单 # 前缀，取到"用法"
# 之后的全部内容；grep 无匹配时不要因 pipefail 让脚本以非零退出）
usage() {
  sed -n '/^# 用法:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo "dev")}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
GOVERSION=$(go version | awk '{print $3}')

LDFLAGS="-s -w \
  -X 'wdp/internal/cli.Version=${VERSION}' \
  -X 'wdp/internal/cli.Commit=${COMMIT}' \
  -X 'wdp/internal/cli.BuildDate=${DATE}' \
  -X 'wdp/internal/cli.GoVersion=${GOVERSION}'"

ALL_TARGETS="linux/amd64 linux/arm64 darwin/arm64 windows/amd64"

# ---- 参数解析 ----

TARGETS=""
EXPLICIT_TARGETS=0

while [ $# -gt 0 ]; do
    case "$1" in
    -h|--help) usage; exit 0 ;;
    all)
        TARGETS="$TARGETS $ALL_TARGETS"
        EXPLICIT_TARGETS=1
        ;;
    */*)
        TARGETS="$TARGETS $1"
        EXPLICIT_TARGETS=1
        ;;
    *)
        echo "error: 无法识别的参数 $1（os/arch 或 all）" >&2; exit 1
        ;;
    esac
    shift
done

# 目标：缺省全平台；去重（all 与显式目标混用时）
if [ "$EXPLICIT_TARGETS" -eq 0 ]; then
    TARGETS="$ALL_TARGETS"
else
    DEDUP=""
    for t in $TARGETS; do
        case " $DEDUP " in *" $t "*) continue ;; esac
        DEDUP="$DEDUP $t"
    done
    TARGETS="$DEDUP"
fi

# ---- 工具函数 ----

# checksum <产物basename> 写入 bin/SHA256SUMS（同名录内去重后追加）
checksum() {
    name=$1
    sums=bin/SHA256SUMS
    tmp="${sums}.tmp"
    : > "$tmp"
    [ -f "$sums" ] && grep -v "  ${name}\$" "$sums" >> "$tmp" || true
    line=$(cd bin && { shasum -a 256 "$name" 2>/dev/null || sha256sum "$name"; })
    echo "$line" >> "$tmp"
    mv "$tmp" "$sums"
}

# prune_sums 清理 SHA256SUMS 中产物已不存在的过期条目（如平台集调整后的
# 旧平台），保持校验和清单与 bin 目录内容一致
prune_sums() {
    sums=bin/SHA256SUMS
    [ -f "$sums" ] || return 0
    tmp="${sums}.tmp"
    : > "$tmp"
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        name=${line##*  }
        [ -n "$name" ] && [ -f "bin/$name" ] && printf '%s\n' "$line" >> "$tmp"
    done < "$sums"
    mv "$tmp" "$sums"
}

# ---- 构建 ----

for target in $TARGETS; do
    os=${target%/*} arch=${target#*/}
    out="bin/wdp-${os}-${arch}"
    [ "$os" = "windows" ] && out="${out}.exe"
    echo "==> build ${os}/${arch} → ${out}"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/wdp
    checksum "$(basename "$out")"
done

prune_sums
