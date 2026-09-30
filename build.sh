#!/bin/bash

# wdp 构建脚本：缺省交叉编译全平台二进制（全量 + 瘦 agent 两档），产出一份多架构 bin 目录
#
# 用法:
#   ./build.sh [选项] [控制端目标 os/arch …]
#
#   控制端目标缺省 = 全平台（ALL_TARGETS）；关键字 all 同样展开全集；显式给出
#   目标时只编译指定平台。产物命名（windows 另带 .exe 后缀），bin/SHA256SUMS
#   记录校验和：
#     全量档  bin/wdp-<os>-<arch>          裸 go build 同构（含 web console）
#     瘦agent档 bin/wdp-agent-<os>-<arch>  独立入口 cmd/wdp-agent：无 console/
#                                          SSH/CLI 命令面（目标机攻击面最小；
#                                          纳管/推装优先下发该产物，缺失回退全量档）
#   全平台集合：linux/amd64 linux/arm64 darwin/arm64 windows/amd64
#
#   多架构 bin 目录是控制端的分发单元：运行的二进制与其他平台二进制互为
#   同级文件，纳管脚本按目标机 uname 自动取同级对应二进制下发——跨平台
#   零配置，什么环境自动运行什么二进制。
#
# 档位（tier）说明（分层方案 P1）：
#   full   缺省产物，含 web console（SQLite + 内嵌前端）；server 与全部 CLI
#          命令面（run/plan/apply/chart/repo/ca/drift/server/agent…）同体
#   agent  瘦 agent：独立入口零 tag，依赖边界由 cmd/wdp-agent/deps_test.go
#          的 go list 断言在 CI 锁死；版本串与全量档同源一致（升级门控按
#          BuildVersion 比较，tier 不进版本串）
#
# 两档即全部产物面。曾有的 --tier cli（-tags wdp_no_console，
# 无 console 的控制端）与 --tier console（-tags wdp_no_cli，仅服务端
# 命令面）已删除：前者省 4.5 MB（console 域），后者只省 0.4 MB 却要维护
# 一对 tag 与 stub 的同步，而"专用服务器上不想要 run/apply"这种诉求真正
# 的收敛手段是 RBAC（internal/web/perm.go），不是编译期裁命令。删掉的两
# 档在仓库里除了 build.sh 自己之外无任何引用——没有消费方的档位就是
# 纯维护成本。
#
# 选项:
#   --tier <档位>     只构建指定档位（full | agent；缺省 = full+agent）
#   --frontend, -f    先构建 frontend/（需 npm），产物拷入 internal/web/static/
#                     经 go:embed 打进二进制；缺省跳过（裸 go build 可编译，
#                     控制台返回引导页，API 不受影响）
#   -h, --help        本帮助
#
# 示例:
#   ./build.sh                          # 全平台全量档+瘦agent档 → bin/ + SHA256SUMS
#   ./build.sh --frontend               # 同上，且内嵌 Web 控制台前端
#   ./build.sh --tier agent             # 只构建瘦 agent 档
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

VERSION_PINNED=${VERSION:+1}
VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo "dev")}
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")
# 未钉版本的开发构建：脏工作区的代码改动 git 元数据看不见（describe/HEAD
# 不变），重建后版本串与旧二进制相同 → agent 升级门控误判「已最新」。
# 把工作区差异哈希并进 COMMIT：内容一变版本串就变（未跟踪文件不参与，
# 与 go build 的输入集合一致按跟踪文件算）。钉了 VERSION 的发布构建
# 不做此处理——发布树应当是干净的，版本串即权威。
if [ -z "$VERSION_PINNED" ] && ! git diff --quiet HEAD >/dev/null 2>&1; then
  DIRTY_HASH=$(git diff HEAD 2>/dev/null | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-7)
  [ -n "$DIRTY_HASH" ] && COMMIT="${COMMIT}.d${DIRTY_HASH}"
fi
DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
GOVERSION=$(go version | awk '{print $3}')

# ldflags 按档位注入 Tier（显示与排障用；刻意不进 BuildVersion——升级
# 门控按版本串比较同源构建，tier 进串会让 full/agent 永远不等，见
# buildinfo.go 的反模式警告）
ldflags() {
  echo "-s -w \
  -X 'wdp/internal/buildinfo.Version=${VERSION}' \
  -X 'wdp/internal/buildinfo.Commit=${COMMIT}' \
  -X 'wdp/internal/buildinfo.BuildDate=${DATE}' \
  -X 'wdp/internal/buildinfo.GoVersion=${GOVERSION}' \
  -X 'wdp/internal/buildinfo.Tier=${1}'"
}

# darwin/amd64 对齐 CI cross-build 矩阵（此前矩阵验证了一个 build.sh
# 缺省不产出的目标，矩阵与产线集合脱节）
ALL_TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

# ---- 参数解析 ----

TARGETS=""
EXPLICIT_TARGETS=0
BUILD_FRONTEND=0
# 缺省档位集 = full + agent（两档即全部产物面）
TIERS="full agent"

while [ $# -gt 0 ]; do
    case "$1" in
    --frontend|-f) BUILD_FRONTEND=1 ;;
    --tier)
        shift
        case "$1" in
        full|agent) TIERS="$1" ;;
        *) echo "error: 未知档位 $1（full | agent）" >&2; exit 1 ;;
        esac
        ;;
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
        echo "error: 无法识别的参数 $1（os/arch、all、--tier、--frontend 或 --help）" >&2; exit 1
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

# 档位定义：前缀（产物名）/ 构建入口 / buildinfo.Tier。两档都零 tag：
# full 与裸 go build ./cmd/wdp 同构（决策：裸构建保持全量）；agent 是独立
# 入口而非 tag——依赖边界由 import 图 + CI 断言保证，tag 写漏只会静默
# 多带，import 越界是硬失败。
tier_prefix() {
  case "$1" in
    full) echo "wdp" ;;
    agent) echo "wdp-agent" ;;
  esac
}
tier_entry() {
  case "$1" in
    agent) echo "./cmd/wdp-agent" ;;
    *) echo "./cmd/wdp" ;;
  esac
}

for tier in $TIERS; do
  prefix=$(tier_prefix "$tier")
  entry=$(tier_entry "$tier")
  for target in $TARGETS; do
    os=${target%/*} arch=${target#*/}
    out="bin/${prefix}-${os}-${arch}"
    [ "$os" = "windows" ] && out="${out}.exe"
    echo "==> build [${tier}] ${os}/${arch} → ${out}"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags "$(ldflags "$tier")" -o "$out" "$entry"
    checksum "$(basename "$out")"
  done
done

prune_sums

# 前端经 go:embed 固化进二进制：构建只产出新 bin/，不触碰正在运行的
# 服务进程——部署侧必须用新二进制重启，控制台才会用上新前端
if [ "$BUILD_FRONTEND" = "1" ]; then
    echo ""
    echo "提示：前端经 go:embed 打进二进制，正在运行的服务需用新 bin/ 产物重启后新控制台才生效。"
fi
