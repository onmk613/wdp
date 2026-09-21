package inventory

import "testing"

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
