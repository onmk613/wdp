#!/bin/bash

# wdp 构建脚本：缺省交叉编译全平台二进制，产出一份多架构 bin 目录
#
# 用法:
#   ./build.sh [选项] [控制端目标 os/arch …]
#
#   控制端目标缺省 = 全平台（ALL_TARGETS）；关键字 all 同样展开全集；显式给出
#   目标时只编译指定平台。产物一律命名 bin/wdp-<os>-<arch>（windows 另带
#   .exe 后缀），bin/SHA256SUMS 记录校验和。
#   全平台集合：linux/amd64 linux/arm64 darwin/arm64 windows/amd64
#
#   多架构 bin 目录是控制端的分发单元：运行的二进制与其他平台二进制互为
#   同级文件，纳管脚本按目标机 uname 自动取同级对应二进制下发——跨平台
#   零配置，什么环境自动运行什么二进制。
#
# 选项:
#   --frontend, -f    先构建 frontend/（需 npm），产物拷入 internal/web/static/
#                     经 go:embed 打进二进制；缺省跳过（裸 go build 可编译，
#                     控制台返回引导页，API 不受影响）
#   -h, --help        本帮助
#
# 示例:
#   ./build.sh                          # 全平台 → bin/wdp-<os>-<arch>[.exe] + SHA256SUMS
#   ./build.sh --frontend               # 同上，且内嵌 Web 控制台前端
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
  -X 'wdp/internal/buildinfo.Version=${VERSION}' \
  -X 'wdp/internal/buildinfo.Commit=${COMMIT}' \
  -X 'wdp/internal/buildinfo.BuildDate=${DATE}' \
  -X 'wdp/internal/buildinfo.GoVersion=${GOVERSION}'"

# darwin/amd64 对齐 CI cross-build 矩阵（此前矩阵验证了一个 build.sh
# 缺省不产出的目标，矩阵与产线集合脱节）
ALL_TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

# ---- 参数解析 ----

TARGETS=""
EXPLICIT_TARGETS=0
BUILD_FRONTEND=0

while [ $# -gt 0 ]; do
    case "$1" in
    --frontend|-f) BUILD_FRONTEND=1 ;;
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
        echo "error: 无法识别的参数 $1（os/arch、all、--frontend 或 --help）" >&2; exit 1
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

# 清理 SHA256SUMS 中产物已不存在的过期条目（如平台集调整后的
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

# ---- 前端（可选：--frontend）----

if [ "$BUILD_FRONTEND" -eq 1 ]; then
    command -v npm >/dev/null 2>&1 || { echo "error: --frontend 需要 npm（Node.js）" >&2; exit 1; }
    [ -d frontend ] || { echo "error: 缺少 frontend/ 目录" >&2; exit 1; }
    echo "==> frontend build (vite)"
    ( cd frontend && npm install --no-fund --no-audit && npm run build )
    # 清掉上一轮产物：只增不删会留下旧 bundle（与 index.html 引用的
    # 哈希名不一致），调试时可能加载到旧代码
    rm -rf internal/web/static/assets internal/web/static/index.html
    mkdir -p internal/web/static
    cp -R frontend/dist/. internal/web/static/
    echo "    embedded $(ls internal/web/static/assets 2>/dev/null | wc -l | tr -d ' ') asset(s)"
fi

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

# 前端经 go:embed 固化进二进制：构建只产出新 bin/，不触碰正在运行的
# 服务进程——部署侧必须用新二进制重启，控制台才会用上新前端
if [ "$BUILD_FRONTEND" = "1" ]; then
    echo ""
    echo "提示：前端经 go:embed 打进二进制，正在运行的服务需用新 bin/ 产物重启后新控制台才生效。"
fi
