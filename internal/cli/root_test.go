package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/config"
	"wdp/internal/i18n"
)

// resetGlobals 恢复全局 flag 变量到内置默认（config.Current 由各自测试显式加载）。
func resetGlobals() {
	gConfig = config.DefaultPath
	gInventories = nil
	gForks = 5
	gTimeout = 0
	gTaskTimeout = 0
	gVerbosity = 0
	gQuiet = false
	gNoColor = false
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
	if err := execRoot(t, "--config", cfg, "version"); err != nil {
		t.Fatal(err)
	}
	if gInventories == nil || gInventories[0] != "hosts/prod.yaml" || gForks != 20 || gTaskTimeout != 300 {
		t.Fatalf("配置未生效: inv=%v forks=%d task_timeout=%d", gInventories, gForks, gTaskTimeout)
	}
	if gVerbosity != 1 || !gNoColor {
		t.Fatalf("bool 配置未生效: verbose=%d no_color=%v", gVerbosity, gNoColor)
	}
}

func TestExplicitFlagBeatsConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "wdp.cfg")
	if err := os.WriteFile(cfg, []byte(testCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	resetGlobals()
	if err := execRoot(t, "--config", cfg, "--forks", "8", "-i", "other.yaml", "version"); err != nil {
		t.Fatal(err)
	}
	if gForks != 8 || gInventories[0] != "other.yaml" {
		t.Fatalf("显式 flag 应覆盖配置: forks=%d inv=%v", gForks, gInventories)
	}
	// 未显式指定的 flag 仍取配置值
	if gVerbosity != 1 {
		t.Fatal("未指定的 flag 应回退配置值")
	}
}

func TestConfigMissingFile(t *testing.T) {
	// 显式 --config 指向不存在的文件：报错
	resetGlobals()
	if err := execRoot(t, "--config", "/nonexistent/wdp.cfg", "version"); err == nil {
		t.Fatal("显式指定的配置文件不存在应报错")
	}

	// 默认路径不存在：静默跳过，保持内置默认
	resetGlobals()
	t.Chdir(t.TempDir()) // cwd 无 wdp.cfg
	if err := execRoot(t, "version"); err != nil {
		t.Fatal(err)
	}
	if gForks != 5 || len(gInventories) != 1 || gInventories[0] != "inventory.yaml" || gVerbosity != 0 || gNoColor {
		t.Fatalf("缺省应保持内置默认: forks=%d inv=%v", gForks, gInventories)
	}
}

// TestRootHelpGrouped 根帮助按命令组分类展示（部署/应用包/安全/代理/运维/其它），
// 且每个命令都归属某个组。
func TestRootHelpGrouped(t *testing.T) {
	i18n.Resolve("en") // 断言英文组标题，与语言无关的稳定性
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(io.Discard)
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// 组标题全部出现
	for _, title := range []string{
		"Deployment Commands:", "Chart & Package Commands:", "Security & Trust Commands:",
		"Agent Commands:", "Operations & Records Commands:", "Other Commands:",
	} {
		if !strings.Contains(out, title) {
			t.Fatalf("帮助缺少分组标题 %q:\n%s", title, out)
		}
	}
	// 无未分组命令（"Additional Commands" 不应出现）
	if strings.Contains(out, "Additional Commands") {
		t.Fatalf("存在未分组命令:\n%s", out)
	}

	// 组 ↔ 命令归属断言
	want := map[string]string{
		"run": groupDeploy, "adhoc": groupDeploy,
		"new": groupChart, "template": groupChart, "lint": groupChart, "package": groupChart,
		"ca": groupSecurity, "key": groupSecurity,
		"agent":   groupAgent,
		"release": groupOps, "modules": groupOps,
		"version": groupOther,
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
