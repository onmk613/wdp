package inventory_test

import (
	"os"
	"path/filepath"
	"testing"

	"wdp/internal/inventory"

	// 测试样例使用 agent 连接参数键，需注册白名单（与 cli 组合根同路径；
	// 外部测试包避免 import cycle）
	_ "wdp/internal/conn/agentc"
	_ "wdp/internal/conn/sshc"
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
	// 密封：不读真实 ~/.ssh/config（buildHost 会解析补全 SSH 参数）
	t.Setenv("WDP_SSH_CONFIG", "/nonexistent-wdp-ssh-config")
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
			if h.Address != "10.0.0.11" || h.Port != 2222 || h.User != "admin" || h.Conn != "push" {
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

func TestSSHConfigFillsUnspecifiedParams(t *testing.T) {
	// ~/.ssh/config 补全未显式给出的 user/port/key：显式键优先，
	// ssh config 次之，wdp.cfg/内置默认垫底（对齐 OpenSSH 优先级）
	cfgDir := t.TempDir()
	t.Setenv("WDP_SSH_CONFIG", filepath.Join(cfgDir, "config"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config"), []byte(`
Host 10.0.1.10
  User dbuser
  Port 2202
  IdentityFile ~/.ssh/db_key
Host web1
  User shouldnotwin
`), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range inv.Hosts {
		switch h.Name {
		case "db1": // 未显式指定：ssh config 补全
			if h.User != "dbuser" || h.Port != 2202 {
				t.Fatalf("db1 应被 ssh config 补全: %+v", h)
			}
			if len(h.IdentityFiles) != 1 {
				t.Fatalf("db1 IdentityFiles: %v", h.IdentityFiles)
			}
		case "web1": // 显式 user/port：优先于 ssh config
			if h.User != "admin" || h.Port != 2222 || len(h.IdentityFiles) != 0 {
				t.Fatalf("web1 显式键应优先: %+v", h)
			}
		case "web2": // agent 通道：不套用 SSH 参数（预填默认值保持不动）
			if h.User != "root" || h.Port != 22 {
				t.Fatalf("web2 不应受 ssh config 影响: %+v", h)
			}
		}
	}
}

// TestGroupLevelConnKeys 组级连接参数键生效：组 vars 中的 conn/user 等按
// 「主机条目 > 组 vars > wdp.cfg/内置默认」合并，且不进入模板变量域。
func TestGroupLevelConnKeys(t *testing.T) {
	t.Setenv("WDP_SSH_CONFIG", "/nonexistent-wdp-ssh-config")
	sample := `
all:
  vars:
    conn: push
    user: root
    port: 2202

webservers:
  vars:
    port: 2203
  hosts:
    web1: {host: 10.0.0.11}                       # 全用组级
    web2: {host: 10.0.0.12, conn: ssh, port: 22}  # 条目覆盖组级

dbservers:
  hosts:
    db1: {host: 10.0.1.10}                        # 不在该组：不受 webservers 影响
`
	inv, err := inventory.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range inv.Hosts {
		switch h.Name {
		case "web1": // all.vars 生效；子组 vars 覆盖 all.vars
			if h.Conn != "push" || h.User != "root" || h.Port != 2203 {
				t.Fatalf("web1 应取组级参数: %+v", h)
			}
		case "web2": // 主机条目优先于组级
			if h.Conn != "ssh" || h.Port != 22 {
				t.Fatalf("web2 条目应覆盖组级: %+v", h)
			}
		case "db1": // 无组级键时保持内置默认（user 取空 cfg 默认 root）
			if h.Conn != "push" || h.Port != 2202 {
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
	t.Setenv("WDP_SSH_CONFIG", "/nonexistent-wdp-ssh-config")
	sample := `
prod:
  children: [webservers]
  vars:
    user: deploy
webservers:
  hosts:
    web1: {host: 10.0.0.11}
`
	inv, err := inventory.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	h := inv.Hosts[0]
	if h.User != "deploy" {
		t.Fatalf("父组 user 应生效（children 展开）: %+v", h)
	}
}
