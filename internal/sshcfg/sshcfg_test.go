package sshcfg

// ~/.ssh/config 解析器测试：OpenSSH 首匹配语义、通配/取反模式、Include
// 原地展开、Key=Value 写法与 FillFromSSHConfig 优先级。
// 全部经 WDP_SSH_CONFIG 指向临时文件，不触碰真实用户配置（无进程级缓存，
// 每次解析即时读取，t.Setenv 即时生效）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/model"
)

// useConfig 把 WDP_SSH_CONFIG 指向写入给定内容的临时配置文件。
func useConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WDP_SSH_CONFIG", path)
	return path
}

// noConfig 显式声明"无 ssh config"，保证测试不读真实 ~/.ssh/config。
func noConfig(t *testing.T) {
	t.Helper()
	t.Setenv("WDP_SSH_CONFIG", filepath.Join(t.TempDir(), "nonexistent"))
}

func resolveFor(t *testing.T, name, address string) sshConfigParams {
	t.Helper()
	return loadSSHConfigParams(name, address)
}

func TestSSHConfigFirstMatchWins(t *testing.T) {
	useConfig(t, `
Host web*
  User first
  Port 2201
Host web1
  User second
  Port 2202
`)
	p := resolveFor(t, "web1", "web1")
	if p.user != "first" || p.port != 2201 {
		t.Fatalf("首匹配语义失败: %+v", p)
	}
}

func TestSSHConfigWildcardAndNegation(t *testing.T) {
	useConfig(t, `
Host * !bastion !10.9.9.9
  User fleet
  Port 2222
`)
	for _, host := range []string{"web1", "10.0.0.1"} {
		if p := resolveFor(t, host, host); p.user != "fleet" {
			t.Fatalf("%s 应命中: %+v", host, p)
		}
	}
	for _, host := range []string{"bastion", "10.9.9.9"} {
		if p := resolveFor(t, host, host); p.user != "" {
			t.Fatalf("%s 应被取反排除: %+v", host, p)
		}
	}
	// ? 单字符通配
	useConfig(t, "Host web?\n  User q")
	if p := resolveFor(t, "web7", "web7"); p.user != "q" {
		t.Fatalf("?: %+v", p)
	}
}

func TestSSHConfigGlobalBlockAndAccumulate(t *testing.T) {
	// 首个 Host 前的指令为全局块；User/Port 取首个命中（含全局块），
	// IdentityFile 跨全部匹配块按序累积
	useConfig(t, `
User globaluser
IdentityFile ~/.ssh/global_key

Host web*
  User web
  IdentityFile ~/.ssh/web_key

Host *
  User star
  IdentityFile ~/.ssh/star_key
`)
	p := resolveFor(t, "web1", "web1")
	if p.user != "globaluser" {
		t.Fatalf("全局块 User 应最先命中: %+v", p)
	}
	want := []string{expandTilde("~/.ssh/global_key"), expandTilde("~/.ssh/web_key"), expandTilde("~/.ssh/star_key")}
	if len(p.identityFiles) != len(want) {
		t.Fatalf("IdentityFile 累积失败: %v", p.identityFiles)
	}
	for i := range want {
		if p.identityFiles[i] != want[i] {
			t.Fatalf("IdentityFile 顺序错误: %v != %v", p.identityFiles, want)
		}
	}
	// 非匹配主机只拿到全局块 + Host * 的值
	p2 := resolveFor(t, "db1", "db1")
	if p2.user != "globaluser" || len(p2.identityFiles) != 2 {
		t.Fatalf("db1: %+v", p2)
	}
}

func TestSSHConfigMatchBlockSkipped(t *testing.T) {
	useConfig(t, `
Match all
  User matched
  Port 2200
Host *
  User star
`)
	p := resolveFor(t, "web1", "web1")
	if p.user != "star" || p.port != 0 {
		t.Fatalf("Match 块应被跳过: %+v", p)
	}
}

func TestSSHConfigIncludeInlineOrder(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "inc.conf")
	if err := os.WriteFile(inc, []byte("User included\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "config")
	if err := os.WriteFile(main, []byte(`
Host web*
  Include inc.conf
  User late
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WDP_SSH_CONFIG", main)
	// 相对路径按包含文件目录解释；Include 后外层块状态延续
	p := resolveFor(t, "web1", "web1")
	if p.user != "included" {
		t.Fatalf("Include 应原地展开且首值生效: %+v", p)
	}
}

func TestSSHConfigIncludeGlobAndCycle(t *testing.T) {
	dir := t.TempDir()
	a1 := filepath.Join(dir, "a1.conf")
	a2 := filepath.Join(dir, "a2.conf")
	_ = os.WriteFile(a1, []byte("IdentityFile ~/.ssh/a1\n"), 0o600)
	_ = os.WriteFile(a2, []byte("IdentityFile ~/.ssh/a2\n"), 0o600)
	// 自包含环：seen 防护确保只展开一次、不死循环
	_ = os.WriteFile(filepath.Join(dir, "self.conf"), []byte("Include self.conf\nIdentityFile ~/.ssh/self\n"), 0o600)
	useConfig(t, "Include "+dir+"/a*.conf\nInclude "+dir+"/self.conf\n")

	p := resolveFor(t, "web1", "web1")
	if len(p.identityFiles) != 3 {
		t.Fatalf("glob 展开 + 环防护失败: %v", p.identityFiles)
	}
}

func TestSSHConfigSyntaxVariants(t *testing.T) {
	useConfig(t, `
user=equser
PORT=2202
identityFile "/tmp/key with space/id_ed25519"
HOST=x
`)
	p := resolveFor(t, "x", "x")
	if p.user != "equser" || p.port != 2202 {
		t.Fatalf("Key=Value/大小写/引号: %+v", p)
	}
	if len(p.identityFiles) != 1 || p.identityFiles[0] != "/tmp/key with space/id_ed25519" {
		t.Fatalf("引号路径: %v", p.identityFiles)
	}
}

func TestSSHConfigMalformedLinesSkipped(t *testing.T) {
	useConfig(t, `
Host web1
  User
  Port abc
  Port 99999
  User ok
`)
	p := resolveFor(t, "web1", "web1")
	if p.user != "ok" || p.port != 0 {
		t.Fatalf("坏行应跳过不阻断后续指令: %+v", p)
	}
}

func TestSSHConfigNameAndAddressMatch(t *testing.T) {
	useConfig(t, `
Host web1
  User alias
Host 10.0.0.5
  User ip
`)
	// Name 命中
	if p := resolveFor(t, "web1", "10.0.0.9"); p.user != "alias" {
		t.Fatalf("Name 匹配失败: %+v", p)
	}
	// Address 命中
	if p := resolveFor(t, "srv", "10.0.0.5"); p.user != "ip" {
		t.Fatalf("Address 匹配失败: %+v", p)
	}
}

func TestSSHConfigMissingFileAndDisable(t *testing.T) {
	noConfig(t)
	if p := resolveFor(t, "web1", "web1"); p.user != "" || p.port != 0 || len(p.identityFiles) != 0 {
		t.Fatalf("文件缺失应返回零值: %+v", p)
	}
	t.Setenv("WDP_SSH_CONFIG", "none")
	if p := resolveFor(t, "web1", "web1"); p.user != "" {
		t.Fatalf("none 应禁用解析: %+v", p)
	}
}

func TestFillFromSSHConfigPrecedence(t *testing.T) {
	useConfig(t, `
Host 10.0.0.1
  User cfguser
  Port 2222
  IdentityFile ~/.ssh/cfg_key
`)
	newHost := func() *model.Host {
		return &model.Host{Name: "srv", Address: "10.0.0.1", Conn: "ssh", User: "default", Port: 22}
	}

	// 未显式指定：ssh config 覆盖默认值
	h := newHost()
	FillFromSSHConfig(h, false, false, false)
	if h.User != "cfguser" || h.Port != 2222 {
		t.Fatalf("未显式字段应被 ssh config 覆盖: %+v", h)
	}
	if len(h.IdentityFiles) != 1 || !strings.HasSuffix(h.IdentityFiles[0], "cfg_key") {
		t.Fatalf("IdentityFiles: %v", h.IdentityFiles)
	}

	// 显式指定：inventory 键优先，不被覆盖
	h = newHost()
	FillFromSSHConfig(h, true, true, true)
	if h.User != "default" || h.Port != 22 || len(h.IdentityFiles) != 0 {
		t.Fatalf("显式键应保持: %+v", h)
	}

	// 非 SSH 通道：不触碰
	h = &model.Host{Name: "a", Address: "10.0.0.1", Conn: "agent", User: "default", Port: 22}
	FillFromSSHConfig(h, false, false, false)
	if h.User != "default" || h.Port != 22 {
		t.Fatalf("agent 通道不应套用 SSH 参数: %+v", h)
	}
}
