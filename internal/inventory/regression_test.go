package inventory

import (
	"reflect"
	"testing"
)

// TestParseBoolYesWorks 安全修复的通用口径：布尔类连接键的 YAML 1.1 惯用
// 写法（yes/no/on/off）必须严格解析——此前静默当 false 等于关闭安全开关
// （历史案例：host_key_check: yes）。host_key_check 键已随 SSH 通道移除，
// 这里用现存布尔键 tls 锁定同一解析路径。
func TestParseBoolYesWorks(t *testing.T) {
	inv, err := Parse([]byte(`
demo:
  hosts:
    h1: {conn: agent, tls: yes}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Hosts[0].TLS {
		t.Fatal("tls: yes 应为 true（布尔键不得静默当 false）")
	}
}

// TestParseBoolStringTrueWorks 字符串 "true"（模板/其它来源常见）同样生效。
func TestParseBoolStringTrueWorks(t *testing.T) {
	inv, err := Parse([]byte(`
demo:
  hosts:
    h1: {conn: agent, tls: "true"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Hosts[0].TLS {
		t.Fatal("tls: \"true\" 应为 true")
	}
}

// TestParseBoolGarbageRejected 无法解析的值应报错，而非静默关闭。
func TestParseBoolGarbageRejected(t *testing.T) {
	if _, err := Parse([]byte(`
demo:
  hosts:
    h1: {conn: agent, tls: maybe}
`)); err == nil {
		t.Fatal("tls: maybe 应报错")
	}
}

// TestConnectTimeoutStrictParsing connect_timeout 与 port 同纪律严格解析：
// "10x" 这类部分解析值此前宽松取 10，拼错的超时直到连接期才暴露。
func TestConnectTimeoutStrictParsing(t *testing.T) {
	inv, err := Parse([]byte(`
demo:
  hosts:
    h1: {connect_timeout: "30"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if inv.Hosts[0].ConnectTimeoutSec != 30 {
		t.Fatalf("合法字符串超时应解析: %d", inv.Hosts[0].ConnectTimeoutSec)
	}
	if _, err := Parse([]byte(`
demo:
  hosts:
    h1: {connect_timeout: "10x"}
`)); err == nil {
		t.Fatal("connect_timeout: \"10x\" 应报错（部分解析）")
	}
	if _, err := Parse([]byte(`
demo:
  hosts:
    h1: {connect_timeout: 5.5}
`)); err == nil {
		t.Fatal("connect_timeout: 5.5 应报错（非整数）")
	}
}

// TestMergeRawDoesNotMutateInputs mergeRaw 契约回归：合并构造全新 group
// （Hosts 新 map、Children 新切片），入参 a/b 保持只读——此前同名组的
// Hosts/Children 被就地改写，仅靠调用方"用后即弃"侥幸无害。
func TestMergeRawDoesNotMutateInputs(t *testing.T) {
	a := rawInventory{
		"g": {
			Hosts: map[string]map[string]any{
				"h1": {"port": 22},
				"h2": {"port": 23},
			},
			Vars:     map[string]any{"x": 1, "deep": map[string]any{"k": "a"}},
			Children: []string{"c1", "c2"},
		},
	}
	b := rawInventory{
		"g": {
			Hosts: map[string]map[string]any{
				"h2": {"port": 24},
				"h3": {"port": 25},
			},
			Vars:     map[string]any{"y": 2, "deep": map[string]any{"k": "b"}},
			Children: []string{"c2", "c3"},
		},
	}
	out := mergeRaw(a, b)

	wantA := rawInventory{
		"g": {
			Hosts: map[string]map[string]any{
				"h1": {"port": 22},
				"h2": {"port": 23},
			},
			Vars:     map[string]any{"x": 1, "deep": map[string]any{"k": "a"}},
			Children: []string{"c1", "c2"},
		},
	}
	wantB := rawInventory{
		"g": {
			Hosts: map[string]map[string]any{
				"h2": {"port": 24},
				"h3": {"port": 25},
			},
			Vars:     map[string]any{"y": 2, "deep": map[string]any{"k": "b"}},
			Children: []string{"c2", "c3"},
		},
	}
	if !reflect.DeepEqual(a, wantA) {
		t.Fatalf("入参 a 被修改: %+v", a)
	}
	if !reflect.DeepEqual(b, wantB) {
		t.Fatalf("入参 b 被修改: %+v", b)
	}

	// 合并语义保持：主机参数后者覆盖、深合并变量、children 并集保序
	g := out["g"]
	if len(g.Hosts) != 3 || g.Hosts["h2"]["port"] != 24 || g.Hosts["h1"]["port"] != 22 {
		t.Fatalf("主机合并语义异常: %+v", g.Hosts)
	}
	if !reflect.DeepEqual(g.Vars, map[string]any{"x": 1, "y": 2, "deep": map[string]any{"k": "b"}}) {
		t.Fatalf("变量深合并语义异常: %+v", g.Vars)
	}
	if !reflect.DeepEqual(g.Children, []string{"c1", "c2", "c3"}) {
		t.Fatalf("children 并集保序语义异常: %+v", g.Children)
	}

	// 结果与入参解耦：结果的外层 Hosts map 与 Children 均为独立拷贝，
	// 增删主机/改 children 不波及入参（逐主机 vars map 仍共享只读引用，
	// 与修复前的结果构造一致——本函数与调用方均不改写它）
	g.Hosts["h9"] = map[string]any{"port": 99}
	delete(g.Hosts, "h1")
	g.Children[0] = "zz"
	if len(a["g"].Hosts) != 2 || a["g"].Hosts["h1"] == nil || a["g"].Children[0] != "c1" {
		t.Fatalf("修改合并结果波及了入参 a: %+v", a["g"])
	}
}
