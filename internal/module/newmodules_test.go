package module

import (
	"strings"
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
		"name": "node5", "address": "10.0.0.5", "agent_port": 7700, "conn": "agent",
		"groups": []any{"etcd", "db"},
		"vars":   map[string]any{"zone": "az2"},
	}, "")
	if res.Failed {
		t.Fatalf("不应失败: %s", res.Msg)
	}
	h := res.AddHost.Host
	if h.Name != "node5" || h.Address != "10.0.0.5" || h.AgentPort != 7700 || h.Conn != "agent" {
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

// TestLintBareCall 空参数调用的静态拦截：模块键写了但底下没有任何参数
// （如 `set_fact:` 后面内容缺失）运行期必然失败，lint 期即报错。
// 回归场景：AI 生成的 playbook 携带空 set_fact 桩，运行时全主机报
// "set_fact requires at least one key/value pair"，用户只看到
// "run finished with failed hosts"。
func TestLintBareCall(t *testing.T) {
	// set_fact：(any) 型，空参数给出键值对语义的报错
	err := LintBareCall(&SetFactModule{}, map[string]any{}, "")
	if err == nil || !strings.Contains(err.Error(), "at least one key/value pair") {
		t.Fatalf("空 set_fact 应报键值对缺失: %v", err)
	}
	if err := LintBareCall(&SetFactModule{}, map[string]any{"k": "v"}, ""); err != nil {
		t.Fatalf("有键值对应通过: %v", err)
	}
	// setup：声明 "(no arguments)"，裸调用合法
	if err := LintBareCall(&SetupModule{}, map[string]any{}, ""); err != nil {
		t.Fatalf("setup 裸调用应豁免: %v", err)
	}
	// 常规模块：空参数报错，带参数/自由格式通过
	err = LintBareCall(&FileModule{}, map[string]any{}, "")
	if err == nil || !strings.Contains(err.Error(), "no parameters") {
		t.Fatalf("空 file 应报无参数: %v", err)
	}
	if err := LintBareCall(&FileModule{}, map[string]any{"path": "/tmp"}, ""); err != nil {
		t.Fatalf("带参数应通过: %v", err)
	}
	if err := LintBareCall(&ShellModule{}, map[string]any{}, "uptime"); err != nil {
		t.Fatalf("自由格式应通过: %v", err)
	}
	if err := LintBareCall(&ShellModule{}, map[string]any{}, ""); err == nil {
		t.Fatal("空 shell（无命令）应报错")
	}
}
