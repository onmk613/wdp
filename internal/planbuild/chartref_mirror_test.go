package planbuild

// chart 引用任务的 hosts:/values_from: 必须在计划镜像中保留——执行侧展开
// 子 chart 时消费这两个字段（expand.go），丢失会导致 run 与 apply 行为
// 分叉：apply 不过滤主机、不应用 values 覆盖。此测试钉住完整链路：
// 编译 → ResolvedTask → JSON 落盘/加载。

import (
	"os"
	"path/filepath"
	"testing"

	"wdp/internal/inventory"
	"wdp/internal/plan"
)

// TestChartRefHostsAndValuesFromSurviveCompile 验证 hosts/values_from 进入
// ResolvedTask 并在 JSON 往返后保持。
func TestChartRefHostsAndValuesFromSurviveCompile(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: app\nversion: 1.0.0\n")
	write("values.yaml", "app: {name: demo}\n")
	write("deploy.yaml", `
- hosts: webservers
  tasks:
    - chart: {name: jdk, hosts: w1, values_from: [overrides/prod.yaml]}
`)
	write("charts/jdk/chart.yaml", "name: jdk\nversion: 11.0.0\n")
	write("charts/jdk/values.yaml", "version: \"11\"\n")
	write("charts/jdk/deploy.yaml", "- hosts: all\n  tasks:\n    - shell: 'java -version'\n")

	inv, err := inventory.Parse([]byte(compileInv))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(dir, inv, nil, nil, CompileOptions{WdpVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var rt *plan.ResolvedTask
	for _, hp := range p.Hosts {
		for _, task := range hp.Tasks {
			if task.ChartRef == "jdk" {
				rt = task
			}
		}
	}
	if rt == nil {
		t.Fatal("chart ref task not found in compiled plan")
	}
	if rt.ChartHosts != "w1" {
		t.Errorf("ChartHosts = %q, want %q", rt.ChartHosts, "w1")
	}
	if len(rt.ChartValuesFrom) != 1 || rt.ChartValuesFrom[0] != "overrides/prod.yaml" {
		t.Errorf("ChartValuesFrom = %v, want [overrides/prod.yaml]", rt.ChartValuesFrom)
	}

	// JSON 往返（Write/Load）后仍保持——apply 从磁盘加载计划。
	out := filepath.Join(dir, "plan.json")
	if err := p.Write(out); err != nil {
		t.Fatal(err)
	}
	p2, err := plan.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, hp := range p2.Hosts {
		for _, task := range hp.Tasks {
			if task.ChartRef == "jdk" {
				if task.ChartHosts != "w1" || len(task.ChartValuesFrom) != 1 {
					t.Errorf("after JSON round trip: hosts=%q values_from=%v", task.ChartHosts, task.ChartValuesFrom)
				}
				return
			}
		}
	}
	t.Fatal("chart ref task lost after JSON round trip")
}
