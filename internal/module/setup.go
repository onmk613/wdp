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
type SetupModule struct{}

// Name 模块名。
func (m *SetupModule) Name() string { return "setup" }

// Desc 模块说明。
func (m *SetupModule) Desc() string {
	return i18n.T("collect host facts (OS/network/memory/disk)", "采集主机 facts（OS/网络/内存/磁盘）")
}

// setupScript 单次探测脚本：输出 key=value 行（缺失项输出空值而非报错，
// 保证无 /proc、无 os-release 的环境（如 macOS local 演练）也能采到 unknown 家族）。
const setupScript = `echo "hostname=$(hostname 2>/dev/null || echo unknown)"
echo "kernel=$(uname -r 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "default_ipv4=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')"
if [ -f /etc/os-release ]; then . /etc/os-release; fi
echo "os_id=${ID:-unknown}"
echo "os_name=${NAME:-}"
echo "os_version=${VERSION_ID:-}"
echo "cpus=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 0)"
echo "memory_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || sysctl -n hw.memsize 2>/dev/null | awk '{print int($1/1048576)}')"
df -k / 2>/dev/null | awk 'NR==2 {printf "disk_total=%d\ndisk_used=%d\ndisk_avail=%d\ndisk_percent=%s\n", $2*1024, $3*1024, $4*1024, $5}'`

// Run 采集主机 facts（只读，不记 changed）。
func (m *SetupModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	out, bad := rc.exec(setupScript)
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		return Fail("facts 采集失败 rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out.Stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	osFacts := map[string]any{
		"id":      kv["os_id"],
		"name":    kv["os_name"],
		"version": kv["os_version"],
		"family":  osFamily(kv["os_id"]),
	}
	percent := 0
	if p := strings.TrimSuffix(kv["disk_percent"], "%"); p != "" {
		percent = atoi(p)
	}
	diskFacts := map[string]any{
		"total_bytes": int64(atoi(kv["disk_total"])),
		"used_bytes":  int64(atoi(kv["disk_used"])),
		"avail_bytes": int64(atoi(kv["disk_avail"])),
		"use_percent": percent,
	}
	facts := map[string]any{
		"hostname":     kv["hostname"],
		"kernel":       kv["kernel"],
		"arch":         kv["arch"],
		"default_ipv4": kv["default_ipv4"],
		"cpus":         atoi(kv["cpus"]),
		"memory_mb":    atoi(kv["memory_mb"]),
		"os":           osFacts,
		"disk":         diskFacts,
	}
	return &Result{Msg: fmt.Sprintf("facts: %s %s（%s）", kv["hostname"], osFacts["family"], kv["arch"]), Facts: facts}
}

// osFamily 归一系统家族（与 package 模块的包管理器探测口径一致）。
func osFamily(id string) string {
	switch {
	case containsAny(id, "debian", "ubuntu"):
		return "debian"
	case containsAny(id, "rhel", "fedora", "centos", "rocky", "alma", "amazon"):
		return "redhat"
	case containsAny(id, "alpine"):
		return "alpine"
	case containsAny(id, "suse", "opensuse"):
		return "suse"
	}
	return "unknown"
}

func containsAny(s string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// Params 参数文档（setup 无参数）。
func (m *SetupModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "(无参数)", Type: "-", Desc: "采集 os/hostname/cpus/memory_mb/disk/default_ipv4 并入变量域"},
	}
}

// Example 示例任务。
func (m *SetupModule) Example() string {
	return `- name: 采集 facts
  setup:

- name: 引用
  shell: 'echo {{ .os.family }} {{ .cpus }}C {{ .memory_mb }}MB'
`
}
