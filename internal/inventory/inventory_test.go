package inventory_test

import (
	"testing"

	"wdp/internal/inventory"

	// 测试样例使用 agent 连接参数键，需注册白名单（与 cli 组合根同路径；
	// 外部测试包避免 import cycle）
	_ "wdp/internal/connection/agentconn"
)

const sample = `
all:
  vars:
    env: prod

webservers:
  hosts:
    web1: {host: 10.0.0.11, port: 2222, user: admin}
    web2: {conn: agent, agent_url: 'http://10.0.0.12:7602', region: cn}
  vars:
    nginx_port: 8080

dbservers:
  hosts:
    db1: {host: 10.0.1.10}

prod:
  children: [webservers, dbservers]
  vars:
    tier: production
`

func load(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestParseHosts(t *testing.T) {
	inv := load(t)
	if len(inv.Hosts) != 3 {
		t.Fatalf("主机数 %d", len(inv.Hosts))
	}
	for _, h := range inv.Hosts {
		switch h.Name {
		case "web1":
			if h.Address != "10.0.0.11" || h.Port != 2222 || h.User != "admin" || h.Conn != "ssh" {
				t.Fatalf("web1: %+v", h)
			}
		case "web2":
			if h.Conn != "agent" || h.AgentURL != "http://10.0.0.12:7602" {
				t.Fatalf("web2: %+v", h)
			}
			// 非连接键应进入主机变量
			if h.Vars["region"] != "cn" {
				t.Fatalf("web2 vars: %+v", h.Vars)
			}
		case "db1":
			if h.Address != "10.0.1.10" || h.Port != 22 {
				t.Fatalf("db1: %+v", h)
			}
		}
	}
}

func TestVarsMergePriority(t *testing.T) {
	inv := load(t)
	for _, h := range inv.Hosts {
		switch h.Name {
		case "web1":
			// all < 组 < 主机；同时含 magic vars
			if h.Vars["env"] != "prod" || h.Vars["nginx_port"] != 8080 {
				t.Fatalf("web1 vars: %+v", h.Vars)
			}
			if h.Vars["inventory_hostname"] != "web1" {
				t.Fatalf("缺 inventory_hostname")
			}
		case "db1":
			if h.Vars["nginx_port"] != nil {
				t.Fatalf("db1 不应有 nginx_port")
			}
		}
	}
}

func TestSelectPatterns(t *testing.T) {
	inv := load(t)
	cases := []struct {
		pattern string
		want    int
	}{
		{"all", 3},
		{"webservers", 2},
		{"dbservers", 1},
		{"web1", 1},
		{"webservers,dbservers", 3},
		{"all,!webservers", 1},
		{"webservers,!web2", 1},
	}
	for _, c := range cases {
		hosts, err := inv.Select(c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if len(hosts) != c.want {
			t.Fatalf("%s: 选中 %d 台，期望 %d", c.pattern, len(hosts), c.want)
		}
	}
	if _, err := inv.Select("nope"); err == nil {
		t.Fatal("未知模式应报错")
	}
}

func TestChildrenGroupVars(t *testing.T) {
	// 属于 prod 子组的成员应继承其变量（父组变量被子组覆盖方向：父组先应用）
	inv := load(t)
	hosts, err := inv.Select("prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("prod 成员 %d", len(hosts))
	}
	for _, h := range hosts {
		if h.Vars["tier"] != "production" {
			t.Fatalf("%s 缺父组变量 tier: %+v", h.Name, h.Vars)
		}
	}
}
