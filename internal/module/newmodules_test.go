package module

import (
	"testing"

	"wdp/internal/model"
)

// TestSetFactModule set_fact 返回参数整表作为 facts（executor 并入本机域与 store）。
func TestSetFactModule(t *testing.T) {
	m := &SetFactModule{}
	rc := &RunContext{Host: &model.Host{Name: "h1"}}
	res := m.Run(rc, map[string]any{"node_id": "a", "weight": 3}, "")
	if res.Failed {
		t.Fatalf("不应失败: %s", res.Msg)
	}
	if res.Facts["node_id"] != "a" || res.Facts["weight"] != 3 {
		t.Fatalf("facts 内容: %#v", res.Facts)
	}
	if !m.ReadOnly() {
		t.Fatal("set_fact 应声明只读")
	}
	if res := m.Run(rc, nil, ""); !res.Failed {
		t.Fatal("空参数应失败")
	}
}

// TestAddHostModule add_host 字段映射与必填校验。
func TestAddHostModule(t *testing.T) {
	m := &AddHostModule{}
	rc := &RunContext{Host: &model.Host{Name: "h1"}}
	res := m.Run(rc, map[string]any{
		"name": "node5", "address": "10.0.0.5", "port": 2222, "conn": "agent",
		"groups": []any{"etcd", "db"},
		"vars":   map[string]any{"zone": "az2"},
	}, "")
	if res.Failed {
		t.Fatalf("不应失败: %s", res.Msg)
	}
	h := res.AddHost.Host
	if h.Name != "node5" || h.Address != "10.0.0.5" || h.Port != 2222 || h.Conn != "agent" {
		t.Fatalf("主机字段: %#v", h)
	}
	if h.Vars["zone"] != "az2" {
		t.Fatalf("主机变量: %#v", h.Vars)
	}
	if len(res.AddHost.Groups) != 2 {
		t.Fatalf("groups: %#v", res.AddHost.Groups)
	}
	if res := m.Run(rc, map[string]any{"address": "1.2.3.4"}, ""); !res.Failed {
		t.Fatal("缺 name 应失败")
	}
	// name 缺省 address = name
	res = m.Run(rc, map[string]any{"name": "node6"}, "")
	if res.AddHost.Host.Address != "node6" {
		t.Fatalf("address 缺省应等于 name: %#v", res.AddHost.Host)
	}
}

// TestValidateArgsAnyWildcard "(any)" 通配：未知键放行、显式 map 参数类型校验。
func TestValidateArgsAnyWildcard(t *testing.T) {
	if err := ValidateArgs(&SetFactModule{}, map[string]any{"k1": "v", "k2": 1}, ""); err != nil {
		t.Fatalf("任意键应放行: %v", err)
	}
	// add_host 的 vars 是显式 map 参数：非 map 值应拒绝
	if err := ValidateArgs(&AddHostModule{}, map[string]any{
		"name": "n1", "vars": "not-a-map",
	}, ""); err == nil {
		t.Fatal("vars 非 map 应报错")
	}
	if err := ValidateArgs(&AddHostModule{}, map[string]any{
		"name": "n1", "vars": map[string]any{"a": 1},
	}, ""); err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
}
