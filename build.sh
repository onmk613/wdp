#!/bin/sh

# 用法:
#   ./build.sh                    编译当前环境架构        → bin/wdp
#   ./build.sh all                交叉编译全架构          → bin/wdp-<os>-<arch>[.exe]
#   ./build.sh linux/amd64 …      编译指定目标（可多个）  → bin/wdp-<os>-<arch>
#   每次编译产物均记录 SHA256 到 bin/SHA256SUMS（同名录去重更新）

set -e

cd "$(dirname "$0")"
mkdir -p bin

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

# build_one <os> <arch> <输出名>
build_one() {
    os=$1 arch=$2 out=$3
    echo "==> build ${os}/${arch} → ${out}"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/wdp
    checksum "$(basename "$out")"
}

case "${1:-}" in
"")
    # 当前环境架构
    build_one "$(go env GOOS)" "$(go env GOARCH)" "bin/wdp"
    ;;
all)
    : > bin/SHA256SUMS
    for target in $ALL_TARGETS; do
        os=${target%/*} arch=${target#*/}
        out="bin/wdp-${os}-${arch}"
        [ "$os" = "windows" ] && out="${out}.exe"
        build_one "$os" "$arch" "$out"
    done
    echo "==> sha256sum: bin/SHA256SUMS"
    ;;
*)
    # 指定目标列表（os/arch 形式）
    for target in "$@"; do
        case "$target" in
        */*) ;;
        *) echo "error: os/arch -> $target" >&2; exit 1 ;;
        esac
        os=${target%/*} arch=${target#*/}
        out="bin/wdp-${os}-${arch}"
        [ "$os" = "windows" ] && out="${out}.exe"
        build_one "$os" "$arch" "$out"
    done
    ;;
esac
