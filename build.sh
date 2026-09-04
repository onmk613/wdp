#!/bin/bash

# wdp 构建脚本：控制端目标 × 内嵌载荷集自由组合
#
# 用法:
#   ./build.sh [选项] [控制端目标 os/arch …]
#
#   控制端目标缺省 = 当前平台（产物 bin/wdp）；关键字 all = 全架构交叉编译
#   （产物 bin/wdp-<os>-<arch>[.exe]）；显式给出目标时产物同样带平台后缀。
#   全架构集合：linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
#
# 选项:
#   --slim, -s        不内嵌自举载荷（旧默认行为；裸 go build 同效）
#   --embed, -e <集合> 内嵌载荷平台集，逗号分隔；缺省 linux/amd64,darwin/arm64。
#                     与 --slim 互斥
#   -h, --help        本帮助
#
# 示例:
#   ./build.sh                                # 当前平台 + 内嵌 linux/amd64 darwin/arm64
#   ./build.sh --slim                         # 当前平台，不内嵌
#   ./build.sh all --slim                     # 全架构，不内嵌
#   ./build.sh all                            # 全架构控制端，各自内嵌默认载荷集
#   ./build.sh linux/amd64 --embed linux/amd64,linux/arm64
#   ./build.sh --slim darwin/amd64 linux/arm64
#
# 内嵌说明（消除跨平台 push 自举的 [agent].push_binary 配置依赖）：
#   阶段一 交叉编译各目标平台的 slim 二进制（不带 pushembed tag——载荷
#          自身不内嵌，杜绝无限套娃）
#   阶段二 gzip 压缩 + manifest.json（SHA256/字节数/版本）落到
#          internal/pushbin/assets/（gitignore 的生成物）
#   阶段三 以 -tags pushembed 编译控制端，assets 经 go:embed 打入
#   运行时取用优先级：binary_path > [agent].push_binary > 同平台自身 > 内嵌载荷

set -euo pipefail

cd "$(dirname "$0")"
mkdir -p bin

# print usage text from this script
usage() {
  grep '^##' "$0" | sed 's/^## \{0,1\}//';
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

ALL_TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"
DEFAULT_EMBED="linux/amd64,darwin/arm64"

# ---- 参数解析 ----

SLIM=0
EMBED_SPEC=""
TARGETS=""
EXPLICIT_TARGETS=0

while [ $# -gt 0 ]; do
    case "$1" in
    --slim|-s) SLIM=1 ;;
    --embed|-e)
        shift
        [ -n "${1:-}" ] || { echo "error: --embed 需要平台集（如 linux/amd64,linux/arm64）" >&2; exit 1; }
        EMBED_SPEC="$1"
        ;;
    --embed=*) EMBED_SPEC="${1#--embed=}" ;;
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
        echo "error: 无法识别的参数 $1（os/arch、all、--slim 或 --embed）" >&2; exit 1
        ;;
    esac
    shift
done

[ "$SLIM" -eq 0 ] || [ -z "$EMBED_SPEC" ] || { echo "error: --slim 与 --embed 互斥" >&2; exit 1; }

# 控制端目标：缺省当前平台；去重（all 与显式目标混用时）
if [ "$EXPLICIT_TARGETS" -eq 0 ]; then
    TARGETS="$(go env GOOS)/$(go env GOARCH)"
else
    DEDUP=""
    for t in $TARGETS; do
        case " $DEDUP " in *" $t "*) continue ;; esac
        DEDUP="$DEDUP $t"
    done
    TARGETS="$DEDUP"
fi

# 内嵌集：--slim 空；否则 --embed 指定或缺省 DEFAULT_EMBED
EMBED_TARGETS=""
if [ "$SLIM" -ne 1 ]; then
    [ -n "$EMBED_SPEC" ] || EMBED_SPEC="$DEFAULT_EMBED"
    EMBED_TARGETS=$(printf '%s' "$EMBED_SPEC" | tr ',' ' ')
    for t in $EMBED_TARGETS; do
        case "$t" in
        */*) ;;
        *) echo "error: --embed 平台须为 os/arch -> $t" >&2; exit 1 ;;
        esac
    done
fi

# ---- 工具函数 ----

# sha256_of <文件> 输出十六进制摘要（macOS/Linux 双兼容）
sha256_of() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        sha256sum "$1" | awk '{print $1}'
    fi
}

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

# build_one <os> <arch> <输出名> <tag>（tag 空 = slim 构建）
build_one() {
    os=$1 arch=$2 out=$3 tags=$4
    echo "==> build ${os}/${arch}${tags:+ (pushembed)} → ${out}"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath ${tags:+-tags pushembed} -ldflags "$LDFLAGS" -o "$out" ./cmd/wdp
    checksum "$(basename "$out")"
}

# ---- 阶段一/二：内嵌载荷 ----

if [ -n "$EMBED_TARGETS" ]; then
    ASSET_DIR=internal/pushbin/assets
    rm -rf "$ASSET_DIR"
    mkdir -p "$ASSET_DIR"
    manifest_entries=""
    for target in $EMBED_TARGETS; do
        os=${target%/*} arch=${target#*/}
        key="${os}_${arch}"
        tmpbin="bin/wdp-payload-${key}"
        echo "==> payload ${os}/${arch} (slim)"
        CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
            go build -trimpath -ldflags "$LDFLAGS" -o "$tmpbin" ./cmd/wdp
        gzip -9n -c "$tmpbin" > "$ASSET_DIR/${key}.bin.gz"
        sha=$(sha256_of "$tmpbin")
        size=$(wc -c < "$tmpbin" | tr -d ' ')
        manifest_entries="${manifest_entries}{\"platform\":\"${key}\",\"sha256\":\"${sha}\",\"size\":${size},\"version\":\"${VERSION}-${COMMIT}\"},"
        echo "    ${key}: $(wc -c < "$ASSET_DIR/${key}.bin.gz" | tr -d ' ') bytes compressed (raw ${size})"
        rm -f "$tmpbin"
    done
    printf '{"payloads":[%s]}\n' "${manifest_entries%,}" > "$ASSET_DIR/manifest.json"
fi

# ---- 阶段三：控制端 ----

BUILD_TAG=""
[ -n "$EMBED_TARGETS" ] && BUILD_TAG="pushembed"

for target in $TARGETS; do
    os=${target%/*} arch=${target#*/}
    if [ "$EXPLICIT_TARGETS" -eq 0 ]; then
        out="bin/wdp"
    else
        out="bin/wdp-${os}-${arch}"
        [ "$os" = "windows" ] && out="${out}.exe"
    fi
    build_one "$os" "$arch" "$out" "$BUILD_TAG"
done
