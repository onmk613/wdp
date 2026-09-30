package module

// setup 归一化与 subset 的表驱动测试：arch 多词表、os.family 精确表 +
// ID_LIKE 派生系、subset 解析、facts 装配的新旧兼容。

import (
	"strings"
	"testing"
)

func TestArchInfoTable(t *testing.T) {
	cases := []struct {
		raw              string
		goArch, deb, rpm string
		bits             any
	}{
		{"x86_64", "amd64", "amd64", "x86_64", 64},
		{"amd64", "amd64", "amd64", "x86_64", 64},    // FreeBSD/Darwin 称法
		{"aarch64", "arm64", "arm64", "aarch64", 64}, // 常见 arm64
		{"arm64", "arm64", "arm64", "aarch64", 64},   // Darwin
		{"armv7l", "arm", "armhf", "armv7hl", 32},
		{"armv8l", "arm", "armhf", "armv7hl", 32}, // 64 位核 32 位用户态
		{"armv7b", "arm", "armhf", "armv7hl", 32}, // 前缀兜底
		{"ppc64le", "ppc64le", "ppc64el", "ppc64le", 64},
		{"loongarch64", "loong64", "loong64", "loongarch64", 64},
	}
	for _, c := range cases {
		info := archInfo(map[string]string{"arch": c.raw})
		if info["go"] != c.goArch || info["deb"] != c.deb || info["rpm"] != c.rpm {
			t.Errorf("archInfo(%q) = go:%v deb:%v rpm:%v, want %s/%s/%s",
				c.raw, info["go"], info["deb"], info["rpm"], c.goArch, c.deb, c.rpm)
		}
		if info["bits"] != c.bits {
			t.Errorf("archInfo(%q).bits = %v, want %v", c.raw, info["bits"], c.bits)
		}
		if info["raw"] != c.raw {
			t.Errorf("archInfo(%q).raw 应保留原值, got %v", c.raw, info["raw"])
		}
	}
	// 未知架构原值透传（新硬件不致命）
	info := archInfo(map[string]string{"arch": "riscv128"})
	if info["go"] != "riscv128" {
		t.Fatalf("未知架构应透传: %v", info)
	}
	// 包管理器实测优先于映射表（用户态与内核架构不一致时）
	info = archInfo(map[string]string{"arch": "armv8l", "native_deb": "arm64"})
	if info["deb"] != "arm64" {
		t.Fatalf("native_deb 应覆盖映射表: %v", info)
	}
}

func TestOSFamily(t *testing.T) {
	cases := []struct{ id, idLike, system, want string }{
		{"debian", "", "Linux", "debian"},
		{"ubuntu", "debian", "Linux", "debian"},
		{"rocky", "rhel fedora", "Linux", "redhat"},
		{"amzn", "", "Linux", "redhat"},
		// 派生系靠 ID_LIKE 归位（此前子串匹配全部落 unknown）
		{"deepin", "debian", "Linux", "debian"},
		{"kylin", "rhel fedora", "Linux", "redhat"}, // 银河麒麟服务端
		{"uos", "debian", "Linux", "debian"},
		{"anolis", "rhel fedora", "Linux", "redhat"},
		{"openeuler", "", "Linux", "redhat"}, // 精确表直配
		// 家族未知但系统已知
		{"somelos", "", "Darwin", "darwin"},
		{"somelos", "", "FreeBSD", "bsd"},
		{"", "", "", "unknown"},
	}
	for _, c := range cases {
		if got := osFamily(c.id, c.idLike, c.system); got != c.want {
			t.Errorf("osFamily(%q,%q,%q) = %q, want %q", c.id, c.idLike, c.system, got, c.want)
		}
	}
}

func TestResolveSubsets(t *testing.T) {
	cases := []struct {
		args    map[string]any
		hw, net bool
		wantErr bool
	}{
		{nil, true, true, false},                               // 缺省 all
		{map[string]any{"subset": "min"}, false, false, false}, // 只采平台段
		{map[string]any{"subset": "all"}, true, true, false},
		{map[string]any{"subset": "hardware"}, true, false, false},
		{map[string]any{"subset": []any{"network"}}, false, true, false},
		{map[string]any{"subset": []any{"all", "!network"}}, true, false, false},
		{map[string]any{"subset": "hardwere"}, false, false, true}, // 拼错 fail-loud
	}
	for _, c := range cases {
		hw, net, err := resolveSubsets(c.args)
		if c.wantErr {
			if err == nil {
				t.Errorf("subset %v 应报错", c.args)
			}
			continue
		}
		if err != nil || hw != c.hw || net != c.net {
			t.Errorf("subset %v = (%v,%v,%v), want (%v,%v,nil)", c.args, hw, net, err, c.hw, c.net)
		}
	}
}

// TestSetupFactsAssembly 新旧字段装配：旧扁平键保持原形态（存量 playbook
// 不破坏），新增归一字段就位；旧版脚本输出（缺新键）不 panic。
func TestSetupFactsAssembly(t *testing.T) {
	facts := setupWithOutput(t, strings.Join([]string{
		"hostname=h1",
		"fqdn=h1.example.com",
		"system=Linux",
		"kernel=6.1.0",
		"arch=x86_64",
		"userspace_bits=64",
		"euid=0",
		"user_name=root",
		"native_deb=amd64",
		"os_id=deepin",
		"os_id_like=debian",
		"os_version=23.1",
		"os_codename=beige",
		"pkg_mgr=apt-get",
		"service_mgr=systemd",
		"libc=glibc",
		"memory_mb=16000",
		"memory_available_mb=8000",
		"swap_total_mb=4096",
		"cpus=8",
		"cpu_quota=2",
		"disk_total_kb=1000000",
		"disk_avail_kb=400000",
		"disk_percent=60%",
		"default_ipv4_address=10.8.2.101",
		"default_ipv4_interface=eth0",
		"default_ipv4_gateway=10.8.2.1",
	}, "\n"))

	// 旧扁平键兼容
	if facts["arch"] != "x86_64" || facts["memory_mb"] != 16000 || facts["cpus"] != 8 {
		t.Fatalf("旧扁平键应保持: arch=%v memory_mb=%v", facts["arch"], facts["memory_mb"])
	}
	if facts["default_ipv4"] != "10.8.2.101" {
		t.Fatalf("default_ipv4 兼容键: %v", facts["default_ipv4"])
	}
	os := facts["os"].(map[string]any)
	if os["family"] != "debian" || os["major_version"] != 23 || os["id_like"] != "debian" {
		t.Fatalf("os 归一字段: %+v", os)
	}
	ai := facts["arch_info"].(map[string]any)
	if ai["go"] != "amd64" || ai["rpm"] != "x86_64" || ai["bits"] != 64 {
		t.Fatalf("arch_info: %+v", ai)
	}
	if facts["pkg_mgr"] != "apt" { // apt-get 归一
		t.Fatalf("pkg_mgr: %v", facts["pkg_mgr"])
	}
	if facts["is_root"] != true || facts["service_mgr"] != "systemd" || facts["libc"] != "glibc" {
		t.Fatalf("新增字段: %+v", facts)
	}
	disk := facts["disk"].(map[string]any)
	if disk["total_bytes"] != int64(1000000*1024) || disk["use_percent"] != 60 {
		t.Fatalf("disk: %+v", disk)
	}
	mem := facts["memory"].(map[string]any)
	if mem["available_mb"] != 8000 || mem["swap_total_mb"] != 4096 {
		t.Fatalf("memory: %+v", mem)
	}
	net := facts["network"].(map[string]any)["default_ipv4"].(map[string]any)
	if net["gateway"] != "10.8.2.1" || net["interface"] != "eth0" {
		t.Fatalf("network: %+v", net)
	}

	// 旧版 agent 的精简输出（无任何新键）不 panic、字段安全缺省
	old := setupWithOutput(t, "hostname=h\nkernel=k\narch=aarch64\nos_id=ubuntu\ncpus=2\nmemory_mb=1024\n")
	if old["pkg_mgr"] != "" || old["fqdn"] != "" {
		t.Fatalf("缺省字段应为空: %+v", old)
	}
	if old["arch_info"].(map[string]any)["go"] != "arm64" {
		t.Fatalf("旧输出也应给出 arch 归一: %+v", old["arch_info"])
	}
}
