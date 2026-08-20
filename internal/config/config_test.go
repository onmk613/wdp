package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wdp.cfg")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndNormalize(t *testing.T) {
	current = Config{} // 重置
	p := writeCfg(t, `
[inventory]
path = "hosts/prod.yaml"

[run]
forks = 20
task_timeout = 300

[ssh]
user = "deploy"
connect_timeout = 15
host_key_check = true

[agent]
port = 8800

[output]
color = false
`)
	if err := Load(p, true); err != nil {
		t.Fatal(err)
	}
	c := Current()
	if c.InventoryPath() != "hosts/prod.yaml" {
		t.Fatalf("inventory: %s", c.InventoryPath())
	}
	if c.Forks() != 20 || c.Run.TaskTimeout != 300 {
		t.Fatalf("run: %+v", c.Run)
	}
	if c.SSHUser() != "deploy" || c.SSHConnectTimeout() != 15 || !c.SSHHostKeyCheck() {
		t.Fatalf("ssh: %+v", c.SSH)
	}
	if c.AgentPort() != 8800 {
		t.Fatalf("agent: %d", c.AgentPort())
	}
	if c.Color() {
		t.Fatal("color 应为 false")
	}
}

func TestDefaultsOnZero(t *testing.T) {
	current = Config{}
	c := Current()
	if c.Forks() != 5 || c.SSHUser() != "root" || c.SSHConnectTimeout() != 10 {
		t.Fatalf("内置默认异常: %+v", c)
	}
	if c.AgentPort() != 7602 || c.InventoryPath() != "inventory.yaml" {
		t.Fatalf("内置默认异常: %+v", c)
	}
	if !c.SSHHostKeyCheck() || !c.Color() {
		t.Fatal("bool 默认异常（host_key_check 安全默认应开启）")
	}
}

func TestLoadMissingOptional(t *testing.T) {
	current = Config{}
	if err := Load("/nonexistent/wdp.cfg", false); err != nil {
		t.Fatalf("可选路径不存在应静默: %v", err)
	}
	if err := Load("/nonexistent/wdp.cfg", true); err == nil {
		t.Fatal("必需路径不存在应报错")
	}
}

func TestLoadBadTOML(t *testing.T) {
	current = Config{}
	p := writeCfg(t, "not [valid toml ===")
	if err := Load(p, true); err == nil {
		t.Fatal("坏 TOML 应报错")
	}
}
