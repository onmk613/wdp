package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"wdp/internal/i18n"
	"wdp/internal/model"
)

// findCmd 在 root 的直接子命令中按名称查找，未找到则报错。
func findCmd(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("命令 %q 未注册", name)
	return nil
}

// TestCommandTreeStructure 断言命令树：全部顶层命令注册、ca/release 子命令齐全。
func TestCommandTreeStructure(t *testing.T) {
	root := NewRootCmd()

	top := map[string]bool{}
	for _, c := range root.Commands() {
		top[c.Name()] = true
	}
	for _, name := range []string{
		"run", "adhoc", "new", "template", "lint", "package",
		"ca", "scan-ssh", "agent", "release", "modules",
	} {
		if !top[name] {
			t.Fatalf("缺少顶层命令 %q", name)
		}
	}

	// ca 子命令
	caCmd := findCmd(t, root, "ca")
	caSub := map[string]bool{}
	for _, c := range caCmd.Commands() {
		caSub[c.Name()] = true
	}
	for _, name := range []string{"init", "issue", "renew", "show"} {
		if !caSub[name] {
			t.Fatalf("ca 缺少子命令 %q", name)
		}
	}

	// release 子命令
	rel := findCmd(t, root, "release")
	relSub := map[string]bool{}
	for _, c := range rel.Commands() {
		relSub[c.Name()] = true
	}
	for _, name := range []string{"list", "show", "diff"} {
		if !relSub[name] {
			t.Fatalf("release 缺少子命令 %q", name)
		}
	}
}

// TestRootPersistentFlags 断言根命令持久 flag：-i 可重复、--forks/--output/--lang 默认值。
func TestRootPersistentFlags(t *testing.T) {
	root := NewRootCmd()
	pf := root.PersistentFlags()

	inv := pf.Lookup("inventory")
	if inv == nil {
		t.Fatal("缺少 --inventory")
	}
	if inv.Value.Type() != "stringArray" {
		t.Fatalf("-i 类型 = %q, 期望 stringArray（可重复）", inv.Value.Type())
	}

	forks := pf.Lookup("forks")
	if forks == nil {
		t.Fatal("缺少 --forks")
	}
	if forks.DefValue != "5" {
		t.Fatalf("--forks 内置默认 = %q, 期望 5", forks.DefValue)
	}

	out := pf.Lookup("output")
	if out == nil || out.DefValue != "console" {
		t.Fatalf("--output 默认应为 console, flag=%v", out)
	}
	lang := pf.Lookup("lang")
	if lang == nil || lang.DefValue != "auto" {
		t.Fatalf("--lang 默认应为 auto, flag=%v", lang)
	}
}

// TestForksDefaultFromConfig 验证 --forks 未显式指定时取 wdp.cfg 的 run.forks。
func TestForksDefaultFromConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "wdp.cfg")
	if err := os.WriteFile(cfg, []byte("[run]\nforks = 12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resetGlobals()
	// modules 仅列内置模块，不触达 inventory/SSH，安全。
	if err := execRoot(t, "--config", cfg, "modules"); err != nil {
		t.Fatal(err)
	}
	if gForks != 12 {
		t.Fatalf("--forks 未取配置默认: got=%d, 期望 12", gForks)
	}
}

// TestRunCmdFlags 断言 `wdp run` 关键 flag：--check/--diff 布尔缺省 false、
// --phase 缺省 deploy、-f/--set 可重复、-t/-y 简写。
func TestRunCmdFlags(t *testing.T) {
	cmd := newRunCmd()
	f := cmd.Flags()

	for _, name := range []string{"check", "diff"} {
		fl := f.Lookup(name)
		if fl == nil || fl.DefValue != "false" {
			t.Fatalf("--%s 缺省应为 false, flag=%v", name, fl)
		}
	}
	phase := f.Lookup("phase")
	if phase == nil || phase.DefValue != "deploy" {
		t.Fatalf("--phase 缺省应为 deploy, flag=%v", phase)
	}
	for _, name := range []string{"values-file", "set"} {
		fl := f.Lookup(name)
		if fl == nil || fl.Value.Type() != "stringArray" {
			t.Fatalf("--%s 应为可重复 stringArray, flag=%v", name, fl)
		}
	}
	if f.ShorthandLookup("t") == nil {
		t.Fatal("缺少 -t 简写（--tags）")
	}
	if f.ShorthandLookup("y") == nil {
		t.Fatal("缺少 -y 简写（--yes）")
	}
}

// TestAdhocCmdFlags 断言 `wdp adhoc` 关键 flag：-m 缺省 shell、布尔缺省 false、简写。
func TestAdhocCmdFlags(t *testing.T) {
	cmd := newAdhocCmd()
	f := cmd.Flags()

	mod := f.Lookup("module")
	if mod == nil || mod.DefValue != "shell" {
		t.Fatalf("--module 缺省应为 shell, flag=%v", mod)
	}
	for _, name := range []string{"become", "check", "diff"} {
		fl := f.Lookup(name)
		if fl == nil || fl.DefValue != "false" {
			t.Fatalf("--%s 缺省应为 false, flag=%v", name, fl)
		}
	}
	for _, s := range []string{"m", "a", "b"} {
		if f.ShorthandLookup(s) == nil {
			t.Fatalf("缺少 -%s 简写", s)
		}
	}
}

// TestAgentPinClientFpRepeatable 断言 agent 的 --pin-client-fp 可重复。
func TestAgentPinClientFpRepeatable(t *testing.T) {
	fl := newAgentCmd().Flags().Lookup("pin-client-fp")
	if fl == nil || fl.Value.Type() != "stringArray" {
		t.Fatalf("--pin-client-fp 应为可重复 stringArray, flag=%v", fl)
	}
}

// TestHelpPathsDoNotExecute 所有子命令的 --help 路径均安全返回 nil（不触达业务执行）。
func TestHelpPathsDoNotExecute(t *testing.T) {
	i18n.Resolve("en") // 固定语言，避免 auto 依赖环境
	for _, args := range [][]string{
		{"--help"},
		{"run", "--help"},
		{"adhoc", "--help"},
		{"new", "--help"},
		{"template", "--help"},
		{"lint", "--help"},
		{"package", "--help"},
		{"ca", "--help"},
		{"scan-ssh", "--help"},
		{"agent", "--help"},
		{"release", "--help"},
		{"modules", "--help"},
	} {
		if err := execRoot(t, args...); err != nil {
			t.Fatalf("%v 帮助路径应返回 nil，实际: %v", args, err)
		}
	}
}

// strSlicesEqual 按序比较两个字符串切片（忽略 nil 与空切片差异）。
func strSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSplitCSV 覆盖逗号分隔解析：空串、去空白、空项忽略。
func TestSplitCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空串", "", nil},
		{"单值", "a", []string{"a"}},
		{"多值", "a,b,c", []string{"a", "b", "c"}},
		{"去空白", " a, ,b ", []string{"a", "b"}},
		{"空项忽略", "a,,b", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := splitCSV(c.in); !strSlicesEqual(got, c.want) {
				t.Fatalf("splitCSV(%q) = %v, 期望 %v", c.in, got, c.want)
			}
		})
	}
}

// TestParseAdhocArgs 覆盖 adhoc 参数解析：k=v 进入 args、其余拼接 free-form。
func TestParseAdhocArgs(t *testing.T) {
	free, args := parseAdhocArgs("echo hello")
	if free != "echo hello" || len(args) != 0 {
		t.Fatalf("纯 free-form 解析错误: free=%q args=%v", free, args)
	}

	free, args = parseAdhocArgs("cmd=uptime timeout=5 tail -n 3")
	if free != "tail -n 3" {
		t.Fatalf("free = %q, 期望 %q", free, "tail -n 3")
	}
	if args["cmd"] != "uptime" || args["timeout"] != "5" {
		t.Fatalf("args = %v, 期望 cmd=uptime timeout=5", args)
	}

	free, args = parseAdhocArgs("")
	if free != "" || len(args) != 0 {
		t.Fatalf("空串解析错误: free=%q args=%v", free, args)
	}
}

// TestReadLine 覆盖交互确认的逐字节读行：普通行、CRLF、EOF 兜底。
func TestReadLine(t *testing.T) {
	line, err := readLine(strings.NewReader("yes\n"))
	if err != nil || line != "yes" {
		t.Fatalf("普通行: line=%q err=%v", line, err)
	}
	line, err = readLine(strings.NewReader("yes\r\n"))
	if err != nil || line != "yes" {
		t.Fatalf("CRLF: line=%q err=%v", line, err)
	}
	line, err = readLine(strings.NewReader("no")) // EOF 无换行但有内容
	if err != nil || line != "no" {
		t.Fatalf("EOF 有内容: line=%q err=%v", line, err)
	}
	if _, err = readLine(strings.NewReader("")); err != io.EOF {
		t.Fatalf("EOF 无内容应返回 io.EOF, got %v", err)
	}
}

// TestEoptsHosts 覆盖部署记录取首个 play 的 hosts 模式。
func TestEoptsHosts(t *testing.T) {
	if got := eoptsHosts(nil); got != "" {
		t.Fatalf("nil plays = %q, 期望空", got)
	}
	if got := eoptsHosts([]*model.Play{}); got != "" {
		t.Fatalf("空 plays = %q, 期望空", got)
	}
	if got := eoptsHosts([]*model.Play{{Hosts: "web"}, {Hosts: "db"}}); got != "web" {
		t.Fatalf("取首个 play hosts = %q, 期望 %q", got, "web")
	}
}

// TestModuleLabel 覆盖任务展示标签：chart 引用加前缀、普通模块直出。
func TestModuleLabel(t *testing.T) {
	if got := moduleLabel(&model.Task{ChartRef: "common@1.x"}); got != "chart:common@1.x" {
		t.Fatalf("chart 引用标签 = %q", got)
	}
	if got := moduleLabel(&model.Task{Module: "shell"}); got != "shell" {
		t.Fatalf("普通模块标签 = %q", got)
	}
}

// TestBoolLabel 覆盖部署结果布尔标签。
func TestBoolLabel(t *testing.T) {
	if boolLabel(true) != "失败" || boolLabel(false) != "成功" {
		t.Fatalf("boolLabel(true)=%q boolLabel(false)=%q", boolLabel(true), boolLabel(false))
	}
}

// TestSampleDomain 覆盖 template 预览域：追加 inventory_hostname 且不污染原 values。
func TestSampleDomain(t *testing.T) {
	values := map[string]any{"app": "nginx"}
	got := sampleDomain(values, "h1")
	if got["inventory_hostname"] != "h1" {
		t.Fatalf("inventory_hostname = %v, 期望 h1", got["inventory_hostname"])
	}
	if got["app"] != "nginx" {
		t.Fatalf("原键未保留: %v", got)
	}
	if _, ok := values["inventory_hostname"]; ok {
		t.Fatal("sampleDomain 污染了原 values map")
	}
}
