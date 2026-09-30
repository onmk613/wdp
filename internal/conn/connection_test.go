package conn

// EnvKeyAllowed 契约测试：env 键白名单是 ExecRequest.Env 的一部分，
// 所有通道（sshc/local/selfrun）共用此单一实现。回归：selfrun 曾自带
// 一份漏下划线续位的正则，导致 FOO_BAR 经 agent 通道 become 时被静默
// 丢弃而 SSH 通道正常。

import "testing"

func TestEnvKeyAllowed(t *testing.T) {
	ok := []string{"FOO", "FOO_BAR", "_LEAD", "A1_b2", "a"}
	for _, k := range ok {
		if !EnvKeyAllowed(k) {
			t.Errorf("合法键被拒: %q", k)
		}
	}
	bad := []string{
		"", "bad-key", "A B", "1LEAD", "A=B", "A\nB", "FOO;", "$(X)", "ФОУ",
	}
	for _, k := range bad {
		if EnvKeyAllowed(k) {
			t.Errorf("非法键被放行: %q", k)
		}
	}
}
