package playbook

import "testing"

// TestNoLogYesParsedAsTrue 安全修复：no_log: yes（YAML 1.1 惯用写法）
// 必须解析为 true，不能静默当 false 导致输出泄露。
func TestNoLogYesParsedAsTrue(t *testing.T) {
	plays, err := Parse([]byte(`
- name: p
  hosts: all
  tasks:
    - name: secret
      shell: echo hi
      no_log: yes
`))
	if err != nil {
		t.Fatal(err)
	}
	if !plays[0].Tasks[0].NoLog {
		t.Fatal("no_log: yes 应为 true")
	}
}

// TestNoLogGarbageRejected 无法解析的布尔值应报错，而非静默 false。
func TestNoLogGarbageRejected(t *testing.T) {
	_, err := Parse([]byte(`
- name: p
  hosts: all
  tasks:
    - name: secret
      shell: echo hi
      no_log: maybe
`))
	if err == nil {
		t.Fatal("no_log: maybe 应报错")
	}
}

// TestBoolCanonicalForms 各类惯用布尔写法均可解析。
func TestBoolCanonicalForms(t *testing.T) {
	for _, v := range []string{"true", "yes", "on", "1", "True", "YES"} {
		plays, err := Parse([]byte(`
- name: p
  hosts: all
  tasks:
    - name: x
      shell: echo hi
      ignore_errors: ` + v + `
`))
		if err != nil {
			t.Fatalf("ignore_errors: %s 应合法: %v", v, err)
		}
		if !plays[0].Tasks[0].IgnoreErrors {
			t.Fatalf("ignore_errors: %s 应为 true", v)
		}
	}
}
