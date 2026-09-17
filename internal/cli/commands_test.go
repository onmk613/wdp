package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"wdp/internal/config"
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
		"run", "plan", "apply", "adhoc", "schema", "module", "render", "lint", "package",
		"ca", "scan-ssh", "agent", "agentctl", "drift", "release", "inv",
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

	// template 命令组已移除：module/render 为顶层命令，template/version 不复存在
	_ = findCmd(t, root, "module")
	_ = findCmd(t, root, "render")
	for _, gone := range []string{"template", "version"} {
		for _, c := range root.Commands() {
			if c.Name() == gone {
				t.Fatalf("命令 %q 应已删除", gone)
			}
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
}

// TestForksDefaultFromConfig 验证 --forks 未显式指定时取 wdp.cfg 的 run.forks。
func TestForksDefaultFromConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "wdp.cfg")
	if err := os.WriteFile(cfg, []byte("[run]\nforks = 12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resetGlobals()
	// modules 仅列内置模块，不触达 inventory/SSH，安全。
	if err := execRoot(t, "--config", cfg, "module"); err != nil {
		t.Fatal(err)
	}
	if got := config.Current().Run.Forks; got != 12 {
		t.Fatalf("--forks 未取配置默认: got=%d, want 12", got)
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
	for _, args := range [][]string{
		{"--help"},
		{"run", "--help"},
		{"adhoc", "--help"},
		{"lint", "--help"},
		{"module", "--help"},
		{"render", "--help"},
		{"package", "--help"},
		{"ca", "--help"},
		{"scan-ssh", "--help"},
		{"agent", "--help"},
		{"release", "--help"},
	} {
		if err := execRoot(t, args...); err != nil {
			t.Fatalf("%v 帮助路径应返回 nil，实际: %v", args, err)
		}
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
	if boolLabel(true) != "failed" || boolLabel(false) != "ok" {
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

// TestModuleCompletion `wdp module` 补全：候选带模块描述、按前缀过滤、
// 首个参数给出后不再补全（cobra __complete 协议）。
func TestModuleCompletion(t *testing.T) {
	root := NewRootCmd()
	mod := findCmd(t, root, "module")
	if mod.ValidArgsFunction == nil {
		t.Fatal("module 未配置 ValidArgsFunction")
	}

	got, dir := mod.ValidArgsFunction(mod, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, 期望 NoFileComp", dir)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "copy\t") || !strings.Contains(joined, "shell\t") {
		t.Fatalf("候选缺少 copy/shell: %v", got)
	}
	for _, c := range got {
		if !strings.Contains(c, "\t") {
			t.Fatalf("候选 %q 缺少描述", c)
		}
	}

	// 前缀过滤
	got, _ = mod.ValidArgsFunction(mod, nil, "co")
	for _, c := range got {
		if !strings.HasPrefix(c, "co") {
			t.Fatalf("候选 %q 未按前缀 co 过滤", c)
		}
	}
	if len(got) == 0 {
		t.Fatal("前缀 co 应有候选（copy/command）")
	}

	// 参数已齐：不再补全
	if got, dir = mod.ValidArgsFunction(mod, []string{"copy"}, ""); got != nil || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("参数已齐应返回空: %v %v", got, dir)
	}
}
