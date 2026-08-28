package conn

import "testing"

// TestAgentIdleTimeoutMinOrDefault push 空闲超时默认值归一化：
// nil/0 = 内置默认 60；负值 = 禁用；正值原样。
func TestAgentIdleTimeoutMinOrDefault(t *testing.T) {
	cases := []struct {
		name string
		dc   *Defaults
		want int
	}{
		{"nil", nil, 60},
		{"零值", &Defaults{}, 60},
		{"显式 120", &Defaults{AgentIdleTimeoutMin: 120}, 120},
		{"负值禁用", &Defaults{AgentIdleTimeoutMin: -1}, 0},
	}
	for _, c := range cases {
		if got := c.dc.AgentIdleTimeoutMinOrDefault(); got != c.want {
			t.Errorf("%s: 应为 %d，实际 %d", c.name, c.want, got)
		}
	}
}
