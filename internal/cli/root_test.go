package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/config"
)

// resetGlobals 恢复全局 flag 变量与配置到内置默认。
func resetGlobals() {
	config.Reset()
	gInventories = nil
	gVerbosity = 0
	gQuiet = false
}

// execRoot 以给定参数执行根命令（输出丢弃），返回执行错误。
func execRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	return root.Execute()
}

const testCfg = `
[inventory]
path = "hosts/prod.yaml"

[run]
forks = 20
task_timeout = 300
verbose = true

[output]
color = false
`

func TestConfigAppliesToFlags(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "wdp.cfg")
	if err := os.WriteFile(cfg, []byte(testCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	resetGlobals()
	if err := execRoot(t, "--config", cfg, "template", "module"); err != nil {
		t.Fatal(err)
	}
	c := config.Current()
	if gInventories == nil || gInventories[0] != "hosts/prod.yaml" || c.Run.Forks != 20 || c.Run.TaskTimeout != 300 {
		t.Fatalf("配置未生效: inv=%v forks=%d task_timeout=%d", gInventories, c.Run.Forks, c.Run.TaskTimeout)
	}
	if gVerbosity != 1 || c.Color() {
		t.Fatalf("bool 配置未生效: verbose=%d color=%v", gVerbosity, c.Color())
	}
}

func TestExplicitFlagBeatsConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "wdp.cfg")
	if err := os.WriteFile(cfg, []byte(testCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	resetGlobals()
	if err := execRoot(t, "--config", cfg, "--forks", "8", "-i", "other.yaml", "template", "module"); err != nil {
		t.Fatal(err)
	}
	if config.Current().Run.Forks != 8 || gInventories[0] != "other.yaml" {
		t.Fatalf("显式 flag 应覆盖配置: forks=%d inv=%v", config.Current().Run.Forks, gInventories)
	}
	// 未显式指定的 flag 仍取配置值
	if gVerbosity != 1 {
		t.Fatal("未指定的 flag 应回退配置值")
	}
}

func TestConfigMissingFile(t *testing.T) {
	// 显式 --config 指向不存在的文件：报错
	resetGlobals()
	if err := execRoot(t, "--config", "/nonexistent/wdp.cfg", "template", "module"); err == nil {
		t.Fatal("显式指定的配置文件不存在应报错")
	}

	// 默认路径不存在：静默跳过，保持内置默认
	resetGlobals()
	t.Chdir(t.TempDir()) // cwd 无 wdp.cfg
	if err := execRoot(t, "template", "module"); err != nil {
		t.Fatal(err)
	}
	if config.Current().Forks() != 5 || len(gInventories) != 1 || gInventories[0] != "inventory.yaml" || gVerbosity != 0 || !config.Current().Color() {
		t.Fatalf("缺省应保持内置默认: forks=%d inv=%v", config.Current().Forks(), gInventories)
	}
}

// TestRootHelpGrouped 根帮助按命令组分类展示（部署/应用包/安全/代理/运维/其它），
// 且每个命令都归属某个组。
func TestRootHelpGrouped(t *testing.T) {
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(io.Discard)
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 组标题全部出现（version 已由 cobra 内置 --version 提供，不再有 other 组）
	for _, title := range []string{
		"Deployment", "Package", "Security", "Agent", "Operations",
	} {
		if !strings.Contains(out, title) {
			t.Fatalf("帮助缺少分组标题 %q:\n%s", title, out)
		}
	}
	// 无未分组命令（"Additional Commands" 不应出现）
	if strings.Contains(out, "Additional Commands") {
		t.Fatalf("存在未分组命令:\n%s", out)
	}

	// 组 ↔ 命令归属断言（直接断言组 ID 字面量，与 root.go 的分组定义对账）
	want := map[string]string{
		"run": "deploy", "adhoc": "deploy",
		"template": "chart", "lint": "chart", "package": "chart",
		"ca":       "security",
		"scan-ssh": "security",
		"agent":    "agent",
		"drift":    "ops", "release": "ops", "inventory": "ops",
	}
	for _, c := range root.Commands() {
		gid, ok := want[c.Name()]
		if !ok {
			continue // help/completion 等框架命令
		}
		if c.GroupID != gid {
			t.Fatalf("命令 %s 分组 %q，期望 %q", c.Name(), c.GroupID, gid)
		}
	}
}
