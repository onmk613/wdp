package inventory_test

import (
	"testing"

	"wdp/internal/inventory"

	// 测试样例使用 agent 连接参数键，需注册白名单（与 cli 组合根同路径；
	// 外部测试包避免 import cycle）
	_ "wdp/internal/conn/agentc"
)

const sample = `
all:
  vars:
    env: prod

webservers:
  hosts:
    web1: {host: 10.0.0.11}
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
			if h.Address != "10.0.0.11" || h.Conn != "agent" {
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
			if h.Address != "10.0.1.10" {
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

// TestGroupLevelConnKeys 组级连接参数键生效：组 vars 中的 conn/agent_port
// 等按「主机条目 > 组 vars > wdp.cfg/内置默认」合并，且不进入模板变量域。
func TestGroupLevelConnKeys(t *testing.T) {
	sample := `
all:
  vars:
    conn: agent
    agent_port: 7700

webservers:
  vars:
    agent_port: 7701
  hosts:
    web1: {host: 10.0.0.11}                                 # 全用组级
    web2: {host: 10.0.0.12, conn: local, agent_port: 7602}   # 条目覆盖组级

dbservers:
  hosts:
    db1: {host: 10.0.1.10}                                  # 不在该组：不受 webservers 影响
`
	inv, err := inventory.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range inv.Hosts {
		switch h.Name {
		case "web1": // all.vars 生效；子组 vars 覆盖 all.vars
			if h.Conn != "agent" || h.AgentPort != 7701 {
				t.Fatalf("web1 应取组级参数: %+v", h)
			}
		case "web2": // 主机条目优先于组级
			if h.Conn != "local" || h.AgentPort != 7602 {
				t.Fatalf("web2 条目应覆盖组级: %+v", h)
			}
		case "db1": // 无组级键时保持内置默认
			if h.Conn != "agent" || h.AgentPort != 7700 {
				t.Fatalf("db1 应继承 all.vars: %+v", h)
			}
		}
		// 连接参数键不得出现在模板变量域（避免既是参数又是变量）
		if _, ok := h.Vars["conn"]; ok {
			t.Fatalf("%s: conn 不应进入变量域", h.Name)
		}
	}
}

// TestChildrenGroupConnKeys children 展开的组同样参与连接键合并。
func TestChildrenGroupConnKeys(t *testing.T) {
	sample := `
prod:
  children: [webservers]
  vars:
    agent_port: 7702
webservers:
  hosts:
    web1: {host: 10.0.0.11}
`
	inv, err := inventory.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	h := inv.Hosts[0]
	if h.AgentPort != 7702 {
		t.Fatalf("父组 agent_port 应生效（children 展开）: %+v", h)
	}
}

// TestSelectLimitedKeepsInputSlice SelectLimited 的 copy 语义回归：过滤
// 结果不得改写传入切片的底层数组——调用方把 Select 的结果数组留作它用
// （如缓存全量清单）时，就地复用会被静默改写成过滤后的内容。
func TestSelectLimitedKeepsInputSlice(t *testing.T) {
	inv := load(t)
	hosts, err := inv.Select("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) < 2 {
		t.Fatalf("样例应至少有两台主机: %d", len(hosts))
	}
	backup := make([]string, len(hosts))
	for i, h := range hosts {
		backup[i] = h.Name
	}
	limited, err := inv.SelectLimited("all", "web1")
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Name != "web1" {
		t.Fatalf("limit 应收窄到 web1: %+v", limited)
	}
	// 原切片内容应保持全量（不被 limit 过滤就地改写）
	got := make([]string, len(hosts))
	for i, h := range hosts {
		got[i] = h.Name
	}
	if len(got) != len(backup) {
		t.Fatalf("原切片长度被改写: %d != %d", len(got), len(backup))
	}
	for i := range backup {
		if got[i] != backup[i] {
			t.Fatalf("原切片内容被改写: [%d] %s != %s", i, got[i], backup[i])
		}
	}
}
