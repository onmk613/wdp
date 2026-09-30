package module

import (
	"fmt"
	"strconv"
	"strings"
	"wdp/internal/i18n"
)

func init() {
	Register(&SetupModule{})
}

// SetupModule 采集主机 facts 并并入变量域。
//
// 采集分层（subset 参数控制）：platform（恒采：主机名/系统/架构/OS/包
// 管理器等轻量探测）+ hardware（内存/磁盘/CPU，涉 df 与 /proc）+
// network（默认路由，涉 ip/route）。缺省 all；`subset: min` 只采 platform
// ——频繁采集或大批量场景省掉 df/route 探测。
type SetupModule struct{}

func (m *SetupModule) Name() string { return "setup" }

// ReadOnly 采集 facts 不产生目标机变更。
func (m *SetupModule) ReadOnly() bool { return true }

func (m *SetupModule) Desc() string {
	return i18n.T("Gather host facts (OS/architecture/memory/disk/network, normalized)", "采集主机 facts（系统/架构/内存/磁盘/网络，已归一化）")
}

// setupScriptPlatform 恒采段：一条 exec 内的轻量探测（uname/os-release/
// id/command -v），不碰 df 与路由表。LC_ALL=C 固定输出形态防本地化错位；
// os-release 在子 shell 内 source（其变量名如 NAME 会污染外层，个别发行
// 版还含非 POSIX 语法）；每段独立兜底，任一探测失败只让对应字段缺省。
const setupScriptPlatform = `export LC_ALL=C LANG=C
echo "hostname=$(hostname 2>/dev/null || echo unknown)"
echo "fqdn=$(hostname -f 2>/dev/null || hostname 2>/dev/null || echo unknown)"
echo "system=$(uname -s 2>/dev/null)"
echo "kernel=$(uname -r 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "userspace_bits=$(getconf LONG_BIT 2>/dev/null || echo 0)"
echo "euid=$(id -u 2>/dev/null || echo -1)"
echo "user_name=$(id -un 2>/dev/null || echo unknown)"
echo "uptime_seconds=$(awk '{print int($1)}' /proc/uptime 2>/dev/null || echo 0)"
echo "native_deb=$(dpkg --print-architecture 2>/dev/null)"
echo "native_rpm=$(rpm -E '%{_arch}' 2>/dev/null)"
echo "native_apk=$(apk --print-arch 2>/dev/null)"
( if [ -r /etc/os-release ]; then . /etc/os-release
  elif [ -r /usr/lib/os-release ]; then . /usr/lib/os-release
  elif [ -r /etc/alpine-release ]; then ID=alpine; NAME="Alpine Linux"; VERSION_ID=$(cat /etc/alpine-release 2>/dev/null)
  elif [ -r /etc/redhat-release ]; then ID=rhel; NAME=$(cat /etc/redhat-release 2>/dev/null); VERSION_ID=$(echo "$NAME" | sed -n 's/.*release \([0-9][0-9.]*\).*/\1/p')
  fi
  echo "os_id=${ID:-unknown}"
  echo "os_id_like=${ID_LIKE:-}"
  echo "os_name=${NAME:-}"
  echo "os_pretty=${PRETTY_NAME:-}"
  echo "os_version=${VERSION_ID:-}"
  echo "os_codename=${VERSION_CODENAME:-}" )
pkg_mgr=""
for c in apt-get dnf yum apk zypper pacman brew; do
  if command -v "$c" >/dev/null 2>&1; then pkg_mgr="$c"; break; fi
done
[ -n "$pkg_mgr" ] && echo "pkg_mgr=$pkg_mgr"
if [ -d /run/systemd/system ]; then echo "service_mgr=systemd"
elif command -v launchctl >/dev/null 2>&1; then echo "service_mgr=launchd"
elif command -v rc-status >/dev/null 2>&1; then echo "service_mgr=openrc"
else echo "service_mgr=sysvinit"; fi
case "$(uname -s 2>/dev/null)" in
  Darwin|*BSD) echo "libc=libSystem" ;;
  *) if ldd --version 2>&1 | head -n 1 | grep -qi musl; then echo "libc=musl"; else echo "libc=glibc"; fi ;;
esac
exit 0`

// setupScriptHardware 硬件段：内存（/proc/meminfo 缺失时 sysctl 兜底，
// 显式判空——`||` 链在 awk 成功但无匹配行时不触发回退）、CPU（cgroup
// 限额单独给出，容器里 nproc≠实际配额）、根文件系统（df -Pk POSIX 输出
// 不折行；有 timeout 命令时限时防挂死的 NFS 拖住整个 setup）。
const setupScriptHardware = `mem_total=$(awk '/^MemTotal:/{print int($2/1024)}' /proc/meminfo 2>/dev/null)
mem_avail=$(awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo 2>/dev/null)
mem_free=$(awk '/^MemFree:/{print int($2/1024)}' /proc/meminfo 2>/dev/null)
swap_total=$(awk '/^SwapTotal:/{print int($2/1024)}' /proc/meminfo 2>/dev/null)
swap_free=$(awk '/^SwapFree:/{print int($2/1024)}' /proc/meminfo 2>/dev/null)
[ -n "$mem_total" ] || mem_total=$(sysctl -n hw.memsize 2>/dev/null | awk '{print int($1/1048576)}')
echo "memory_mb=${mem_total:-0}"
echo "memory_available_mb=${mem_avail:-${mem_free:-0}}"
echo "memory_free_mb=${mem_free:-0}"
echo "swap_total_mb=${swap_total:-0}"
echo "swap_free_mb=${swap_free:-0}"
echo "cpus=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)"
if [ -r /sys/fs/cgroup/cpu.max ]; then
  echo "cpu_quota=$(awk '{if($1=="max"||$2==0)print 0;else print int($1/$2)}' /sys/fs/cgroup/cpu.max)"
fi
DF="df -Pk"
command -v timeout >/dev/null 2>&1 && DF="timeout 5 df -Pk"
$DF / 2>/dev/null | awk 'NR==2 {printf "disk_total_kb=%d\ndisk_used_kb=%d\ndisk_avail_kb=%d\ndisk_percent=%s\n", $2, $3, $4, $5}'
exit 0`

// setupScriptNetwork 默认路由段：按 src/dev/via 关键字定位（字段位置随
// 有无网关漂移，按下标取 src 在直连路由下会取到 "uid"）；无 ip 命令的
// 环境（macOS/BusyBox）用 route get 兜底。
const setupScriptNetwork = `netline=$(ip -4 route get 1.1.1.1 2>/dev/null | head -n 1)
if [ -n "$netline" ]; then
  echo "$netline" | awk '{a="";d="";g="";for(i=1;i<=NF;i++){if($i=="src")a=$(i+1);if($i=="dev")d=$(i+1);if($i=="via")g=$(i+1)};print "default_ipv4_address=" a;print "default_ipv4_interface=" d;print "default_ipv4_gateway=" g}'
else
  ifc=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
  [ -n "$ifc" ] && echo "default_ipv4_interface=$ifc"
  gw=$(route -n get default 2>/dev/null | awk '/gateway:/{print $2}')
  [ -n "$gw" ] && echo "default_ipv4_gateway=$gw"
  addr=$(ipconfig getifaddr "$ifc" 2>/dev/null)
  [ -n "$addr" ] && echo "default_ipv4_address=$addr"
fi
exit 0`

func (m *SetupModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "(no arguments)", Type: "-", Desc: i18n.T("no arguments gathers all of the information below (default behavior)", "不带参数即采集下列全部信息（缺省行为）")},
		{Name: "subset", Type: "list", Desc: i18n.T("subset of facts to gather, to control probe cost: min is platform/OS/architecture/package manager only (no df / route); hardware adds memory/disk/CPU; network adds the default route; a ! prefix negates (e.g. [hardware, !network]). Defaults to all", "采集子集，控制探测成本：min 仅平台/系统/架构/包管理器（不跑 df / route）；hardware 追加内存/磁盘/CPU；network 追加默认路由；! 前缀取反（如 [hardware, !network]）。缺省 all"),
			Enum: []string{"min", "all", "hardware", "network", "!hardware", "!network"}},
	}
}

func (m *SetupModule) Example() string {
	return i18n.T(`- name: gather everything (default)
  setup:

- name: lightweight probe (no df / route)
  setup: {subset: min}

- name: platform + network, skip disk probes
  setup: {subset: [network]}

- name: use normalized architecture aliases in download URLs
  shell: echo "go={{ .arch_info.go }} deb={{ .arch_info.deb }} rpm={{ .arch_info.rpm }}"
`, `- name: 全量采集（缺省）
  setup:

- name: 轻量探测（不跑 df / route）
  setup: {subset: min}

- name: 平台 + 网络，跳过磁盘探测
  setup: {subset: [network]}

- name: 下载地址里用归一化的架构别名
  shell: echo "go={{ .arch_info.go }} deb={{ .arch_info.deb }} rpm={{ .arch_info.rpm }}"
`)
}

// resolveSubsets 解析 subset 参数为 (hardware, network)。缺省（无参数）
// all；给出任一正向段即"只采点名段"（hardware ≠ all-减-network 的显式
// 写法，而就是只采 hardware）；!段做减法，配合 all 用（`[all, !network]`）。
// 未知段名返回错误（拼错静默当 all 会让用户以为省了探测）。
func resolveSubsets(args map[string]any) (bool, bool, error) {
	toks, _ := argStrList(args, "subset")
	if len(toks) == 0 {
		return true, true, nil
	}
	hw, net := false, false
	for _, t := range toks {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "":
			continue
		case "all":
			hw, net = true, true
		case "min":
			hw, net = false, false
		case "hardware":
			hw = true
		case "network":
			net = true
		case "!hardware":
			hw = false
		case "!network":
			net = false
		default:
			return false, false, fmt.Errorf("setup: unknown subset %q (supported: min/all/hardware/network/!hardware/!network)", t)
		}
	}
	return hw, net, nil
}

// Run 采集 facts（只读，不记 changed）。脚本分段按 subset 拼装，段内
// 每项探测独立兜底；整体失败（rc!=0）才 Fail。
func (m *SetupModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	hw, net, err := resolveSubsets(args)
	if err != nil {
		return Fail("%v", err)
	}
	script := setupScriptPlatform
	if hw {
		script += "\n" + setupScriptHardware
	}
	if net {
		script += "\n" + setupScriptNetwork
	}
	out, bad := rc.exec(script)
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		return Fail("facts collection failed rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	kv := map[string]string{}
	for line := range strings.SplitSeq(out.Stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return &Result{Msg: fmt.Sprintf("facts: %s %s (%s)", kv["hostname"], osFamily(kv["os_id"], kv["os_id_like"], kv["system"]), kv["arch"]), Facts: buildFacts(kv)}
}

// buildFacts 把脚本 key=value 输出装配为 facts。兼容口径：旧扁平键
// （arch/memory_mb/cpus/disk.*/default_ipv4/os.*）保持原形态不破坏现有
// playbook；新增能力走新键（arch_info/network/memory/os 扩展字段）。
func buildFacts(kv map[string]string) map[string]any {
	osFacts := map[string]any{
		"id":            kv["os_id"],
		"id_like":       kv["os_id_like"],
		"name":          kv["os_name"],
		"pretty":        kv["os_pretty"],
		"version":       kv["os_version"],
		"major_version": majorVersion(kv["os_version"]),
		"codename":      kv["os_codename"],
		"family":        osFamily(kv["os_id"], kv["os_id_like"], kv["system"]),
	}
	diskFacts := map[string]any{
		"total_bytes": kbToBytes(kv["disk_total_kb"]),
		"used_bytes":  kbToBytes(kv["disk_used_kb"]),
		"avail_bytes": kbToBytes(kv["disk_avail_kb"]),
		"use_percent": atoi(strings.TrimSuffix(kv["disk_percent"], "%")),
	}
	// euid 缺失按 -1（非 root）处理：root 守卫的安全默认是"不是 root"，
	// atoi("")==0 会冒充 root 放行系统级任务
	euid := -1
	if kv["euid"] != "" {
		euid = atoi(kv["euid"])
	}
	facts := map[string]any{
		"hostname":       kv["hostname"],
		"fqdn":           kv["fqdn"],
		"system":         kv["system"],
		"kernel":         kv["kernel"],
		"arch":           kv["arch"],
		"arch_info":      archInfo(kv),
		"userspace_bits": atoi(kv["userspace_bits"]),
		"euid":           euid,
		"is_root":        euid == 0,
		"user":           map[string]any{"name": kv["user_name"]},
		"uptime_seconds": int64(atoi(kv["uptime_seconds"])),
		"cpus":           atoi(kv["cpus"]),
		"cpu_quota":      atoi(kv["cpu_quota"]),
		"memory_mb":      atoi(kv["memory_mb"]),
		"memory": map[string]any{
			"total_mb":      atoi(kv["memory_mb"]),
			"available_mb":  atoi(kv["memory_available_mb"]),
			"free_mb":       atoi(kv["memory_free_mb"]),
			"swap_total_mb": atoi(kv["swap_total_mb"]),
			"swap_free_mb":  atoi(kv["swap_free_mb"]),
		},
		"default_ipv4": kv["default_ipv4_address"],
		"network": map[string]any{
			"default_ipv4": map[string]any{
				"address":   kv["default_ipv4_address"],
				"interface": kv["default_ipv4_interface"],
				"gateway":   kv["default_ipv4_gateway"],
			},
		},
		"os":          osFacts,
		"disk":        diskFacts,
		"pkg_mgr":     normalizePkgMgr(kv["pkg_mgr"]),
		"service_mgr": kv["service_mgr"],
		"libc":        kv["libc"],
	}
	return facts
}

// majorVersion 取版本号主段（"8.9"→8，"22.04"→22；非数字回退 0）——
// RHEL/Ubuntu 系模板最高频的判断维度。
func majorVersion(v string) int {
	major, _, _ := strings.Cut(v, ".")
	return atoi(major)
}

// kbToBytes df 的 KB 原值在 Go 侧换算字节（int64：awk double 精度 2^53
// 才失真，字节乘法放 Go 侧则 32 位控制端也不溢出）。
func kbToBytes(kb string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(kb), 10, 64)
	if n < 0 {
		return 0
	}
	return n * 1024
}

// normalizePkgMgr 探测到的命令名归一为生态惯用名（apt-get→apt）。
func normalizePkgMgr(cmd string) string {
	if cmd == "apt-get" {
		return "apt"
	}
	return cmd
}

// ---- arch 归一：uname -m 原值 → 各生态词表别名 ----

// archEntry 一个 uname -m 原值在各生态的别名（go/deb/rpm/apk）、位数与
// arm 变体。表驱动：下游 URL 直接用 {{ .arch_info.go }} 拼下载链接，
// 不再在 playbook 里手写 x86_64→amd64 的映射。
type archEntry struct {
	goArch, deb, rpm, apk, variant string
	bits                           int
}

var archTable = map[string]archEntry{
	// x86
	"x86_64":  {"amd64", "amd64", "x86_64", "x86_64", "", 64},
	"amd64":   {"amd64", "amd64", "x86_64", "x86_64", "", 64}, // FreeBSD/Darwin 称法
	"x86_64h": {"amd64", "amd64", "x86_64", "x86_64", "", 64},
	"x64":     {"amd64", "amd64", "x86_64", "x86_64", "", 64},
	"i386":    {"386", "i386", "i386", "x86", "", 32},
	"i486":    {"386", "i386", "i486", "x86", "", 32},
	"i586":    {"386", "i386", "i586", "x86", "", 32},
	"i686":    {"386", "i386", "i686", "x86", "", 32},
	"i86pc":   {"amd64", "amd64", "x86_64", "x86_64", "", 64}, // illumos
	// arm64
	"aarch64":    {"arm64", "arm64", "aarch64", "aarch64", "v8", 64},
	"aarch64_be": {"arm64", "arm64", "aarch64", "aarch64", "v8", 64},
	"arm64":      {"arm64", "arm64", "aarch64", "aarch64", "v8", 64},
	"arm64e":     {"arm64", "arm64", "aarch64", "aarch64", "v8", 64},
	"armv8l":     {"arm", "armhf", "armv7hl", "armv7", "v8", 32}, // 64 位核跑 32 位用户态
	// arm32
	"armv7l":   {"arm", "armhf", "armv7hl", "armv7", "v7", 32},
	"armv7":    {"arm", "armhf", "armv7hl", "armv7", "v7", 32},
	"armhf":    {"arm", "armhf", "armv7hl", "armv7", "v7", 32},
	"armv6l":   {"arm", "armel", "armv6hl", "armhf", "v6", 32},
	"armv5tel": {"arm", "armel", "armv5tel", "armv5", "v5", 32},
	// riscv/ppc/s390x/loongarch
	"ppc64le":     {"ppc64le", "ppc64el", "ppc64le", "ppc64le", "", 64},
	"powerpc64le": {"ppc64le", "ppc64el", "ppc64le", "ppc64le", "", 64},
	"ppc64":       {"ppc64", "ppc64", "ppc64", "ppc64", "", 64},
	"s390x":       {"s390x", "s390x", "s390x", "s390x", "", 64},
	"riscv64":     {"riscv64", "riscv64", "riscv64", "riscv64", "", 64},
	"mips64el":    {"mips64le", "mips64el", "mips64el", "mips64el", "", 64},
	"loongarch64": {"loong64", "loong64", "loongarch64", "loongarch64", "", 64},
	"sparc64":     {"sparc64", "sparc64", "sparc64v9", "sparc64", "", 64},
}

// archInfo 归一 uname -m：raw 保留原值，go/deb/rpm/apk 给出各生态词表
// 别名（包管理器实测值优先于映射表——用户态架构与内核架构不一致时以
// 实测为准）；未知架构按原值透传（新硬件不致命）。
func archInfo(kv map[string]string) map[string]any {
	raw := kv["arch"]
	r := strings.ToLower(strings.TrimSpace(raw))
	e, ok := archTable[r]
	if !ok {
		switch {
		case strings.HasPrefix(r, "armv8"), strings.HasPrefix(r, "aarch64"):
			e = archTable["aarch64"]
		case strings.HasPrefix(r, "armv7"):
			e = archTable["armv7l"]
		case strings.HasPrefix(r, "armv6"):
			e = archTable["armv6l"]
		default:
			e = archEntry{goArch: r, deb: r, rpm: r, apk: r}
		}
	}
	if v := kv["native_deb"]; v != "" {
		e.deb = v
	}
	if v := kv["native_rpm"]; v != "" {
		e.rpm = v
	}
	if v := kv["native_apk"]; v != "" {
		e.apk = v
	}
	info := map[string]any{
		"raw":     raw,
		"go":      e.goArch,
		"deb":     e.deb,
		"rpm":     e.rpm,
		"apk":     e.apk,
		"variant": e.variant,
	}
	if e.bits > 0 {
		info["bits"] = e.bits
	}
	return info
}

// osFamily 系统家族归一：发行版 ID 精确表优先 → ID_LIKE 分词（deepin/
// uos/kylin/anolis 等派生发行版靠它归位）→ uname -s（Darwin/BSD）。
// 此前按 ID 子串匹配：派生系（ID 不含主系子串）全部落 unknown。
var osIDFamily = map[string]string{
	"debian": "debian", "ubuntu": "debian", "raspbian": "debian",
	"deepin": "debian", "uos": "debian",
	// kylin 这类双系发行版（桌面=debian 系、服务器=RHEL 系）不进精确表：
	// ID_LIKE 在目标机上说的是实话，交给它判定
	"rhel": "redhat", "redhat": "redhat", "centos": "redhat", "rocky": "redhat",
	"almalinux": "redhat", "ol": "redhat", "oraclelinux": "redhat",
	"fedora": "redhat", "amzn": "redhat", "amazon": "redhat",
	"openeuler": "redhat", "anolis": "redhat", "rhel_like": "redhat",
	"alpine": "alpine",
	"suse":   "suse", "opensuse": "suse", "sles": "suse", "opensuse-leap": "suse",
	"arch": "arch", "archlinux": "arch", "manjaro": "arch",
	"gentoo": "gentoo",
}

func osFamily(id, idLike, system string) string {
	if f, ok := osIDFamily[strings.ToLower(strings.TrimSpace(id))]; ok {
		return f
	}
	for tok := range strings.FieldsSeq(strings.ToLower(idLike)) {
		if f, ok := osIDFamily[tok]; ok {
			return f
		}
	}
	switch strings.ToLower(strings.TrimSpace(system)) {
	case "darwin":
		return "darwin"
	case "freebsd", "openbsd", "netbsd":
		return "bsd"
	}
	return "unknown"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
