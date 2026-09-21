package conn

import "testing"

// TestConnOrDefault 默认连接类型归一化：空 = agent（常驻 agent 通道是
// 唯一远程通道）；显式值原样生效。
func TestConnOrDefault(t *testing.T) {
	cases := []struct {
		name string
		dc   *Defaults
		want string
	}{
		{"nil", nil, "agent"},
		{"零值", &Defaults{}, "agent"},
		{"显式 local", &Defaults{Conn: "local"}, "local"},
		{"显式 agent", &Defaults{Conn: "agent"}, "agent"},
	}
	for _, c := range cases {
		if got := c.dc.ConnOrDefault(); got != c.want {
			t.Errorf("%s: 应为 %s，实际 %s", c.name, c.want, got)
		}
	}
}

// TestAgentPortOrDefault agent 端口默认值归一化：nil/0 = 7602；正值原样。
func TestAgentPortOrDefault(t *testing.T) {
	cases := []struct {
		name string
		dc   *Defaults
		want int
	}{
		{"nil", nil, 7602},
		{"零值", &Defaults{}, 7602},
		{"显式 7700", &Defaults{AgentPort: 7700}, 7700},
	}
	for _, c := range cases {
		if got := c.dc.AgentPortOrDefault(); got != c.want {
			t.Errorf("%s: 应为 %d，实际 %d", c.name, c.want, got)
		}
	}
}
