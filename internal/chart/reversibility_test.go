package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/model"
)

// §1.3 回归：可逆性评估按相位与实际引用的任务树出发。

// writeAnalyzeChart：deploy 只含 file 模块（可逆）；update.yaml 含 shell；
// charts/unused 含 rm -rf 且从未被引用；charts/used 被 deploy 引用。
func writeAnalyzeChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("chart.yaml", "name: myapp\nversion: 1.0.0\n")
	must("values.yaml", "{}\n")
	must("deploy.yaml", `
- hosts: all
  tasks:
    - file: {path: /data, state: directory}
    - chart: used
`)
	must("update.yaml", `
- hosts: all
  tasks:
    - name: nuke
      shell: 'rm -rf /data'
`)
	must("charts/used/chart.yaml", "name: used\nversion: 1.0.0\n")
	must("charts/used/values.yaml", "{}\n")
	must("charts/used/deploy.yaml", `
- hosts: all
  tasks:
    - shell: 'systemctl restart used'
`)
	must("charts/unused/chart.yaml", "name: unused\nversion: 1.0.0\n")
	must("charts/unused/values.yaml", "{}\n")
	must("charts/unused/deploy.yaml", `
- hosts: all
  tasks:
    - shell: 'rm -rf /everything'
`)
	return dir
}

// TestAnalyzeByPhase：声明 release 的自定义相位评估该相位的任务清单，
// 不再误用 deploy.yaml（漏报修复）。
func TestAnalyzeByPhase(t *testing.T) {
	c, err := Load(writeAnalyzeChart(t))
	if err != nil {
		t.Fatal(err)
	}
	// deploy 相位：file 可逆 + used 子 chart 的 shell 不可逆；
	// 未引用的 unused 不计入
	r := c.Analyze("deploy")
	if r.Reversible != 1 {
		t.Fatalf("deploy 可逆任务应只含 file: %+v", r)
	}
	if r.Irreversible != 1 {
		t.Fatalf("deploy 不可逆应只含 used.shell（unused 未被引用）: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Examples, ";"), "used.") {
		t.Fatalf("不可逆示例应指向 used 子任务: %v", r.Examples)
	}
	// update 相位：shell rm -rf 必须出现在评估里（此前评估的是 deploy.yaml，
	// 用户按 Y 时看到的是"全部可逆"的错误安全感）
	ru := c.Analyze("update")
	if ru.Irreversible != 1 || ru.Reversible != 0 {
		t.Fatalf("update 应命中 update.yaml 的 shell: %+v", ru)
	}
	if !strings.Contains(strings.Join(ru.Examples, ";"), "nuke") {
		t.Fatalf("update 不可逆示例: %v", ru.Examples)
	}
	// uninstall 相位没有 uninstall.yaml：零任务
	if rx := c.Analyze("uninstall"); rx.Irreversible != 0 || rx.Reversible != 0 {
		t.Fatalf("未知/缺失相位应零任务: %+v", rx)
	}
}

// TestAnalyzeUnreferencedSubchartNotCounted：未被 chart: 引用的子 chart
// 即使含 rm -rf 也不计入（虚报修复）。
func TestAnalyzeUnreferencedSubchartNotCounted(t *testing.T) {
	c, err := Load(writeAnalyzeChart(t))
	if err != nil {
		t.Fatal(err)
	}
	// 直接改 deploy 去掉引用再评
	c.Deploy[0].Tasks = c.Deploy[0].Tasks[:1] // 只留 file
	r := c.Analyze("deploy")
	if r.Irreversible != 0 {
		t.Fatalf("未被引用的 unused 子 chart 不应计入不可逆: %+v", r)
	}
}

// TestAnalyzeTasksFromPhase：chart: 引用带 tasks_from 时评估对应入口相位。
func TestAnalyzeTasksFromPhase(t *testing.T) {
	dir := writeAnalyzeChart(t)
	purge := func(p string) { os.Remove(p) }
	purge(filepath.Join(dir, "charts", "used", "deploy.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "charts", "used", "deploy.yaml"), []byte(
		"- hosts: all\n  tasks:\n    - copy: {content: x, dest: /etc/used.conf}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "charts", "used", "purge.yaml"), []byte(
		"- hosts: all\n  tasks:\n    - shell: 'rm -rf /used'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte(
		"- hosts: all\n  tasks:\n    - chart: used\n      tasks_from: purge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Analyze("deploy")
	if r.Irreversible != 1 || r.Reversible != 0 {
		t.Fatalf("tasks_from=purge 的入口任务应被评估: %+v", r)
	}
}

// TestAnalyzeCycleGuard：自引用/互引用不栈溢出（深度上限 + 已访问集合）。
func TestAnalyzeCycleGuard(t *testing.T) {
	a := &Chart{Meta: Meta{Name: "a", Version: "1.0.0"}}
	b := &Chart{Meta: Meta{Name: "b", Version: "1.0.0"}}
	a.Subs = map[string]*Chart{"b": b}
	b.Subs = map[string]*Chart{"a": a}
	a.Deploy = []*model.Play{{Tasks: []*model.Task{
		{Module: "shell", FreeForm: "x"},
		{ChartRef: "b"},
	}}}
	b.Deploy = []*model.Play{{Tasks: []*model.Task{
		{ChartRef: "a"},
	}}}
	done := make(chan *Reversibility, 1)
	go func() { done <- a.Analyze("deploy") }()
	select {
	case r := <-done:
		if r.Irreversible != 1 {
			t.Fatalf("环引用下评估应终止且 shell 计入不可逆: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("环引用导致评估未终止（栈溢出风险）")
	}
}
