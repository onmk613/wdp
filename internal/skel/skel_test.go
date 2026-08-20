package skel

import (
	"context"
	"io"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/connection"
	_ "wdp/internal/connection/localconn" // 注册 local 连接工厂（演练主机）
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/render"
	"wdp/internal/report"
)

const localInv = `
all:
  hosts:
    local1: {conn: local}
`

// loadAndLint 加载并校验生成的骨架。
func loadAndLint(t *testing.T, root string) (*chart.Chart, map[string]any) {
	t.Helper()
	c, err := chart.Load(root)
	if err != nil {
		t.Fatalf("chart.Load(%s) 失败: %v", root, err)
	}
	values, err := c.BuildValues(nil, nil)
	if err != nil {
		t.Fatalf("BuildValues 失败: %v", err)
	}
	if err := c.ValidateRequired(values); err != nil {
		t.Fatalf("required 校验失败: %v", err)
	}
	for _, issue := range chart.Lint(c, values) {
		if issue.Level == chart.ERROR {
			t.Fatalf("lint 发现错误: %s", issue)
		}
	}
	return c, values
}

// runCheckDeploy 以 check 模式在本机演练完整部署（生成物的质量门）。
func runCheckDeploy(t *testing.T, c *chart.Chart, values map[string]any) {
	t.Helper()
	inv, err := inventory.Parse([]byte(localInv))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := render.NewEngine(c.CollectHelpers())
	if err != nil {
		t.Fatal(err)
	}
	rep := report.NewConsole(io.Discard, false, -1)
	ex := executor.New(inv, connection.NewManager(), rep, executor.Options{
		Forks: 2, CheckMode: true, Chart: c, Values: values, Engine: eng,
		BaseDir: c.Dir, Phase: "deploy", WdpVersion: "test",
	})
	if ex.Run(context.Background(), c.Deploy) {
		t.Fatal("check 演练不应失败")
	}
}

// TestScaffoldBasic 最小骨架：lint 通过 + check 演练通过。
func TestScaffoldBasic(t *testing.T) {
	root, err := Scaffold(t.TempDir(), "demo-basic", false)
	if err != nil {
		t.Fatal(err)
	}
	c, values := loadAndLint(t, root)
	runCheckDeploy(t, c, values)
}

// TestScaffoldFull 全能力骨架：lint 通过 + check 演练通过。
// 覆盖：策略/hook/delegate_to/run_once/loop_control/block-rescue/group_by/
// 子 chart 版本约束/新内置模块/output 控制——任何能力回归都会在此暴露。
func TestScaffoldFull(t *testing.T) {
	root, err := Scaffold(t.TempDir(), "demo-full", true)
	if err != nil {
		t.Fatal(err)
	}
	c, values := loadAndLint(t, root)
	runCheckDeploy(t, c, values)
}

// TestScaffoldRefuseOverwrite 已有 chart.yaml 时拒绝覆盖。
func TestScaffoldRefuseOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scaffold(dir, "dup-app", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(dir, "dup-app", false); err == nil {
		t.Fatal("应拒绝覆盖已存在的应用包")
	}
}

// TestModuleSnippet 模块用法片段输出。
func TestModuleSnippet(t *testing.T) {
	s, err := ModuleSnippet("copy")
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Fatal("copy 模块应有用法说明")
	}
	if _, err := ModuleSnippet("no-such-mod"); err == nil {
		t.Fatal("未知模块应报错")
	}
}
