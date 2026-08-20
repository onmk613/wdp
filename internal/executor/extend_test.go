package executor

// P0-P5 新能力的执行器集成测试：delegate_to / run_once / hook 生命周期 /
// group_by 动态组 / chart 脚本模块 / 跨批次 register 延续 / 自定义 loop_var。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/render"
)

// TestDelegateTo 委托执行：命令发到目标主机连接，结果归属原主机。
func TestDelegateTo(t *testing.T) {
	ex, rep := setup(t, okExec)
	if ex.Inv.HostByName("h2") == nil {
		t.Fatal("测试 inventory 缺 h2")
	}
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{{
			Name: "delegate", Module: "shell", FreeForm: "echo from-lb",
			DelegateTo: "h2",
		}},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	names := map[string]bool{}
	for _, f := range allFakes() {
		names[f.Hostname()] = true
	}
	if !names["h2"] {
		t.Fatalf("delegate_to 应在 h2 上执行，实际连接: %v", names)
	}
	if !strings.Contains(rep.joined(), "h1 delegate") {
		t.Fatalf("结果应归属 h1:\n%s", rep.joined())
	}
}

// TestRunOnce 整批只执行一次，register 结果复制到全部主机。
func TestRunOnce(t *testing.T) {
	ex, rep := setup(t, okExec)
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "init", Module: "shell", FreeForm: "init-once", RunOnce: true, Register: "initres"},
			{Name: "use", Module: "shell", FreeForm: "use"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	initCount := 0
	for _, f := range allFakes() {
		for _, r := range f.ExecLog {
			if strings.Contains(r.Script, "init-once") {
				initCount++
			}
		}
	}
	if initCount != 1 {
		t.Fatalf("run_once 应只执行 1 次，实际 %d", initCount)
	}
	out := rep.joined()
	if !strings.Contains(out, "h1 use") || !strings.Contains(out, "h2 use") {
		t.Fatalf("后续任务应在全部主机执行:\n%s", out)
	}
}

// TestLoopVarName 自定义循环变量名（loop_control.loop_var）。
func TestLoopVarName(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{{
			Name: "loop", Module: "shell", FreeForm: "echo {{ .idx }}",
			Loop: []any{"x", "y"}, LoopVar: "idx",
		}},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	joined := ""
	for _, f := range allFakes() {
		for _, r := range f.ExecLog {
			joined += r.Script + "|"
		}
	}
	if !strings.Contains(joined, "echo x") || !strings.Contains(joined, "echo y") {
		t.Fatalf("loop_var 应注入 idx: %s", joined)
	}
}

// TestHookLifecycle hook 相位分离：pre 在主任务前、post 在成功后执行；
// 相位不匹配的 hook 被跳过。
func TestHookLifecycle(t *testing.T) {
	ex, rep := setup(t, okExec)
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Name: "pre", Module: "shell", FreeForm: "echo pre", Hook: "pre_install"},
			{Name: "main", Module: "shell", FreeForm: "echo main"},
			{Name: "post", Module: "shell", FreeForm: "echo post", Hook: "post_install"},
			{Name: "uninst-only", Module: "shell", FreeForm: "echo nope", Hook: "pre_uninstall"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	var order []string
	for _, f := range allFakes() {
		for _, r := range f.ExecLog {
			switch {
			case strings.Contains(r.Script, "echo pre"):
				order = append(order, "pre")
			case strings.Contains(r.Script, "echo main"):
				order = append(order, "main")
			case strings.Contains(r.Script, "echo post"):
				order = append(order, "post")
			case strings.Contains(r.Script, "nope"):
				order = append(order, "nope")
			}
		}
	}
	if got := strings.Join(order, ","); got != "pre,main,post" {
		t.Fatalf("hook 顺序应为 pre,main,post，实际 %q", got)
	}
}

// TestHookPostSkippedOnFailure 主任务失败时 post_install 不执行。
func TestHookPostSkippedOnFailure(t *testing.T) {
	script := func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "boom") {
			return connection.ExecResult{Code: 1}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	}
	ex, _ := setup(t, script)
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Name: "pre", Module: "shell", FreeForm: "echo pre", Hook: "pre_install"},
			{Name: "fail", Module: "shell", FreeForm: "boom"},
			{Name: "post", Module: "shell", FreeForm: "echo post", Hook: "post_install"},
		},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("应失败")
	}
	scripts := ""
	for _, f := range allFakes() {
		for _, r := range f.ExecLog {
			scripts += r.Script
		}
	}
	if strings.Contains(scripts, "echo post") {
		t.Fatal("主任务失败后 post_install 不应执行")
	}
	if !strings.Contains(scripts, "echo pre") {
		t.Fatal("pre_install 应已执行")
	}
}

// TestGroupByDynamicGroup 动态分组：play1 建组，play2 通配引用。
func TestGroupByDynamicGroup(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "os-release") {
			return connection.ExecResult{Code: 0, Stdout: "os_id=debian\nos_name=Debian\nos_version=12\n"}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{
		{
			Hosts: "webservers",
			Tasks: []*model.Task{
				{Module: "setup"},
				{Name: "group", Module: "group_by", FreeForm: "os_{{ .os.family }}"},
			},
		},
		{
			Hosts: "os_*",
			Tasks: []*model.Task{{Name: "family-task", Module: "shell", FreeForm: "echo family"}},
		},
	}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	out := rep.joined()
	if !strings.Contains(out, "h1 family-task") || !strings.Contains(out, "h2 family-task") {
		t.Fatalf("第二个 play 应选中动态组主机:\n%s", out)
	}
	if g := ex.Inv.GroupsMap()["os_debian"]; len(g) != 2 {
		t.Fatalf("动态组应有 2 成员，实际 %v", g)
	}
}

// TestCrossBatchRegister serial 分批下 register 跨批次延续。
func TestCrossBatchRegister(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0, Stdout: "done\n"}, nil
	})
	plays := []*model.Play{{
		Hosts:  "webservers",
		Serial: "1",
		Tasks: []*model.Task{
			{Name: "mark", Module: "shell", FreeForm: "echo m", Register: "mark"},
			{Name: "use", Module: "shell", FreeForm: "echo use-{{ .mark.stdout }}"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	uses := 0
	for _, f := range allFakes() {
		for _, r := range f.ExecLog {
			if strings.Contains(r.Script, "use-done") {
				uses++
			}
		}
	}
	if uses != 2 {
		t.Fatalf("register 应跨批次可用（两台各 1 次 use），实际 %d", uses)
	}
}

// TestScriptModule chart 自带脚本模块：JSON 结果契约 + changed:false 尊重 + 参数环境变量注入。
func TestScriptModule(t *testing.T) {
	setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		// 模拟执行上传的脚本模块：校验参数环境变量并回显脚本自身的 JSON 输出
		if strings.Contains(req.Script, ".wdp-mod-") {
			if req.Env["WDP_MODULE_ARGS"] == "" {
				return connection.ExecResult{Code: 1, Stderr: "missing WDP_MODULE_ARGS"}, nil
			}
			if req.Env["WDP_FREE_FORM"] != "extra-args" {
				return connection.ExecResult{Code: 1, Stderr: "missing WDP_FREE_FORM"}, nil
			}
			return connection.ExecResult{
				Code:   0,
				Stdout: "{\"changed\": false, \"failed\": false, \"msg\": \"script-mod-ok\"}\n",
			}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	})
	dir := t.TempDir()
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: t\nversion: 0.1.0\n")
	write("deploy.yaml", "- hosts: all\n  tasks: []\n")
	write("modules/mycheck", "#!/bin/sh\necho '{\"changed\": false, \"failed\": false, \"msg\": \"script-mod-ok\"}'\n")

	c, err := chart.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := render.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	inv := parseTestInv(t)
	rep := &captureReporter{}
	ex := New(inv, connection.NewManager(), rep, Options{
		Forks: 2, Chart: c, Values: map[string]any{}, Engine: eng, BaseDir: dir,
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{{
			Name: "custom", Module: "mycheck",
			Args:     map[string]any{"k": "v"},
			FreeForm: "extra-args",
		}},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("脚本模块应执行成功:\n%s", rep.joined())
	}
	if !strings.Contains(rep.joined(), "script-mod-ok") {
		t.Fatalf("脚本模块结果未生效:\n%s", rep.joined())
	}
	if rep.recaps["h1"].Changed != 0 || rep.recaps["h1"].Ok != 1 {
		t.Fatalf("changed:false 声明应被尊重: %+v", rep.recaps["h1"])
	}
}
