package model

import (
	"testing"
)

// TestSecret 覆盖 Secret 的取值优先级：envKey > direct（含 "env:" 前缀）> 直通。
func TestSecret(t *testing.T) {
	cases := []struct {
		name   string
		direct string
		envKey string
		setEnv map[string]string // 本用例先设置的变量（用于 t.Setenv）
		want   string
	}{
		{"envKey 优先", "direct", "WDP_TEST_SECRET_A", map[string]string{"WDP_TEST_SECRET_A": "from-env"}, "from-env"},
		{"envKey 存在但变量未设置", "direct", "WDP_TEST_SECRET_UNSET", nil, ""},
		{"direct env: 前缀", "env:WDP_TEST_SECRET_B", "", map[string]string{"WDP_TEST_SECRET_B": "prefix-val"}, "prefix-val"},
		{"direct env: 前缀但变量未设置", "env:WDP_TEST_SECRET_MISSING", "", nil, ""},
		{"direct 直通", "plain-value", "", nil, "plain-value"},
		{"空 direct", "", "", nil, ""},
		{"envKey 优先级高于 direct env: 前缀", "env:WDP_TEST_SECRET_A", "WDP_TEST_SECRET_C",
			map[string]string{"WDP_TEST_SECRET_A": "wrong", "WDP_TEST_SECRET_C": "right"}, "right"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.setEnv {
				t.Setenv(k, v)
			}
			if got := Secret(c.direct, c.envKey); got != c.want {
				t.Fatalf("Secret(%q, %q) = %q, 期望 %q", c.direct, c.envKey, got, c.want)
			}
		})
	}
}

// TestParseBool 覆盖布尔解析的全部接受类型与大小写/空白容错，以及非法值报错。
func TestParseBool(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    bool
		wantErr bool
	}{
		{"bool true", true, true, false},
		{"bool false", false, false, false},
		{"int 1", 1, true, false},
		{"int 0", 0, false, false},
		{"int 2 非法", 2, false, true},
		{"float64 1", 1.0, true, false},
		{"float64 0", 0.0, false, false},
		{"float64 2 非法", 2.0, false, true},

		{"字符串 true", "true", true, false},
		{"字符串 TRUE 大小写", "TRUE", true, false},
		{"字符串 yes", "yes", true, false},
		{"字符串 YES 大小写", "YES", true, false},
		{"字符串 on", "on", true, false},
		{"字符串 1", "1", true, false},
		{"字符串 false", "false", false, false},
		{"字符串 FALSE 大小写", "FALSE", false, false},
		{"字符串 no", "no", false, false},
		{"字符串 off", "off", false, false},
		{"字符串 0", "0", false, false},
		{"字符串带空白", "  Yes  ", true, false},

		{"非法 garbage", "garbage", false, true},
		{"非法 yes 之外的词", "yesyes", false, true},
		{"非法 maybe", "maybe", false, true},
		{"非法空串", "", false, true},
		{"nil", nil, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseBool(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseBool(%v) 期望报错，却返回 %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBool(%v) 不应报错: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseBool(%v) = %v, 期望 %v", c.in, got, c.want)
			}
		})
	}
}

// TestHostCloneDeepCopy 验证 Clone 深拷贝 Vars：修改副本不影响原主机。
func TestHostCloneDeepCopy(t *testing.T) {
	h := &Host{
		Name:    "web1",
		Address: "10.0.0.1",
		Port:    22,
		Vars:    map[string]any{"app": "nginx", "port": 80},
	}
	c := h.Clone()

	// 值字段一致
	if c.Name != h.Name || c.Address != h.Address || c.Port != h.Port {
		t.Fatalf("值字段拷贝不一致: %+v vs %+v", c, h)
	}
	// Vars 是独立 map 实例（引用不同）
	if &c.Vars == &h.Vars {
		t.Fatal("Clone 的 Vars 与原主机共享同一 map 引用")
	}
	// 副本包含全部键
	if c.Vars["app"] != "nginx" || c.Vars["port"] != 80 {
		t.Fatalf("副本 Vars 内容不完整: %v", c.Vars)
	}

	// 修改副本不影响原主机
	c.Vars["app"] = "httpd"
	c.Name = "changed"
	if h.Vars["app"] != "nginx" {
		t.Fatalf("修改副本 Vars 影响了原主机: %v", h.Vars)
	}
	if h.Name != "web1" {
		t.Fatal("修改副本标量字段影响了原主机")
	}
}

// TestHostCloneNilVars 验证 Vars 为 nil 时克隆结果仍为 nil。
func TestHostCloneNilVars(t *testing.T) {
	c := (&Host{Name: "solo"}).Clone()
	if c.Vars != nil {
		t.Fatalf("nil Vars 克隆后应为 nil，实际 %v", c.Vars)
	}
	if c.Name != "solo" {
		t.Fatalf("Name 克隆错误: %q", c.Name)
	}
}

// TestGroupBasicFields 覆盖 Group 结构体的基本字段装配。
func TestGroupBasicFields(t *testing.T) {
	g := &Group{
		Name:      "webservers",
		HostNames: []string{"web1", "web2"},
		Hosts:     []*Host{{Name: "web1"}, {Name: "web2"}},
		Vars:      map[string]any{"env": "prod"},
		Children:  []string{"db"},
	}
	if g.Name != "webservers" {
		t.Fatalf("Name = %q", g.Name)
	}
	if len(g.HostNames) != 2 || g.HostNames[0] != "web1" || g.HostNames[1] != "web2" {
		t.Fatalf("HostNames = %v", g.HostNames)
	}
	if len(g.Hosts) != 2 || g.Hosts[1].Name != "web2" {
		t.Fatalf("Hosts = %v", g.Hosts)
	}
	if g.Vars["env"] != "prod" {
		t.Fatalf("Vars = %v", g.Vars)
	}
	if len(g.Children) != 1 || g.Children[0] != "db" {
		t.Fatalf("Children = %v", g.Children)
	}
}
