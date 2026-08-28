package inventory

// ExpandSpecRange 的区间展开测试：尾段简写、完整写法、端口保留、
// 含连字符主机名不误判、跨度上限与非法输入报错。

import (
	"strings"
	"testing"
)

func expand(t *testing.T, spec string) []string {
	t.Helper()
	out, err := ExpandSpecRange(spec)
	if err != nil {
		t.Fatalf("%q: %v", spec, err)
	}
	return out
}

func TestExpandSpecRangeSuffix(t *testing.T) {
	got := expand(t, "10.8.2.101-104")
	if len(got) != 4 || got[0] != "10.8.2.101" || got[3] != "10.8.2.104" {
		t.Fatalf("尾段简写展开错误: %v", got)
	}
}

func TestExpandSpecRangeFullForm(t *testing.T) {
	got := expand(t, "10.8.2.101-10.8.2.103")
	if len(got) != 3 || got[2] != "10.8.2.103" {
		t.Fatalf("完整写法展开错误: %v", got)
	}
	// 前三段不一致 → 报错
	if _, err := ExpandSpecRange("10.8.2.101-10.8.3.104"); err == nil {
		t.Fatal("前三段不一致应报错")
	}
}

func TestExpandSpecRangeKeepsPort(t *testing.T) {
	got := expand(t, "10.8.2.101-102:2222")
	if len(got) != 2 || got[0] != "10.8.2.101:2222" || got[1] != "10.8.2.102:2222" {
		t.Fatalf("端口未保留: %v", got)
	}
}

func TestExpandSpecRangeHostnameWithDash(t *testing.T) {
	// 含连字符的普通主机名：前缀不是 IP → 原样单个返回，不误判
	for _, spec := range []string{"web-1", "build-agent", "10.8.2.101"} {
		got := expand(t, spec)
		if len(got) != 1 || got[0] != spec {
			t.Fatalf("%q 不应被展开: %v", spec, got)
		}
	}
}

func TestExpandSpecRangeLimits(t *testing.T) {
	if _, err := ExpandSpecRange("10.8.2.104-101"); err == nil || !strings.Contains(err.Error(), "<") {
		t.Fatalf("右端小于左端应报错: %v", err)
	}
	if _, err := ExpandSpecRange("10.8.2.1-300"); err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("跨度超限应报错: %v", err)
	}
	// 恰好 256（合法上限）
	got := expand(t, "10.8.2.0-255")
	if len(got) != 256 {
		t.Fatalf("/24 展开应为 256: %d", len(got))
	}
}

func TestHostsFromSpecsRange(t *testing.T) {
	hosts, err := HostsFromSpecs([]string{"10.8.2.101-102", "web-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("展开后主机数: %d (%v)", len(hosts), hosts)
	}
	if hosts[0].Address != "10.8.2.101" || hosts[2].Name != "web-1" {
		t.Fatalf("主机字段: %#v", hosts)
	}
}

func TestParseHostSpec(t *testing.T) {
	cases := []struct {
		spec    string
		address string
		port    int
		wantErr bool
	}{
		{"10.0.0.1", "10.0.0.1", 22, false},
		{"web1.example.com", "web1.example.com", 22, false},
		{"10.0.0.1:2222", "10.0.0.1", 2222, false},
		{"[10.0.0.1]:2222", "10.0.0.1", 2222, false},
		{"::1", "::1", 22, false},
		{"[::1]:2222", "::1", 2222, false},
		{"10.0.0.1:abc", "", 0, true},
		{"10.0.0.1:", "", 0, true},
		{"10.0.0.1:0", "", 0, true},
		{"10.0.0.1:70000", "", 0, true},
		{"host:22:33", "", 0, true},
	}
	for _, c := range cases {
		h, err := parseHostSpec(c.spec, nil)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错，得到 %+v", c.spec, h)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if h.Address != c.address || h.Port != c.port || h.Conn != "ssh" || h.Name != c.spec {
			t.Errorf("%q: got %+v", c.spec, h)
		}
	}
}
