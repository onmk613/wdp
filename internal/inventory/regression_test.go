package inventory

import "testing"

// TestHostKeyCheckYesWorks 安全修复：host_key_check: yes（YAML 1.1 惯用写法）
// 必须解析为 true——此前静默当 false，等于关闭 SSH 指纹校验。
func TestHostKeyCheckYesWorks(t *testing.T) {
	inv, err := Parse([]byte(`
demo:
  hosts:
    h1: {conn: local, host_key_check: yes}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Hosts[0].HostKeyCheck {
		t.Fatal("host_key_check: yes 应为 true（不得静默关闭指纹校验）")
	}
}

// TestHostKeyCheckStringTrueWorks 字符串 "true"（模板/其它来源常见）同样生效。
func TestHostKeyCheckStringTrueWorks(t *testing.T) {
	inv, err := Parse([]byte(`
demo:
  hosts:
    h1: {conn: local, host_key_check: "true"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Hosts[0].HostKeyCheck {
		t.Fatal("host_key_check: \"true\" 应为 true")
	}
}

// TestHostKeyCheckGarbageRejected 无法解析的值应报错，而非静默关闭校验。
func TestHostKeyCheckGarbageRejected(t *testing.T) {
	if _, err := Parse([]byte(`
demo:
  hosts:
    h1: {conn: local, host_key_check: maybe}
`)); err == nil {
		t.Fatal("host_key_check: maybe 应报错")
	}
}
