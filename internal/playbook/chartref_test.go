package playbook

// chart 引用任务解析回归：简写/map 两形态、同义键冲突拒绝、越权键拒绝、
// CollectChartRefs 递归与去重。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChartRefSimpleForm(t *testing.T) {
	// 简写形态（chart: jdk）已移除：报错并给迁移提示
	_, err := parseTask(map[string]any{
		"chart":  "jdk",
		"values": map[string]any{"version": "17"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "map form") {
		t.Fatalf("简写形态应报错并给迁移提示: %v", err)
	}
	// map 形态 + 平级 values 同样拒绝（配置集中在 map 内）
	_, err = parseTask(map[string]any{
		"chart":  map[string]any{"name": "jdk"},
		"values": map[string]any{"version": "17"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "inside the chart map") {
		t.Fatalf("平级 values 应拒绝: %v", err)
	}
}

func TestChartRefMapForm(t *testing.T) {
	tk, err := parseTask(map[string]any{
		"name": "部署",
		"chart": map[string]any{
			"name":        "nginx",
			"values":      map[string]any{"replicas": 4},
			"values_from": []any{"values/prod.yaml"},
			"hosts":       "webservers",
			"phase":       "deploy",
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ChartRef != "nginx" || tk.ChartVars["replicas"] != 4 {
		t.Fatalf("map 形态解析错误: %+v", tk)
	}
	if len(tk.ChartValuesFrom) != 1 || tk.ChartValuesFrom[0] != "values/prod.yaml" {
		t.Fatalf("values_from 解析错误: %v", tk.ChartValuesFrom)
	}
	if tk.ChartHosts != "webservers" || tk.TasksFrom != "deploy" {
		t.Fatalf("hosts/phase 解析错误: %q %q", tk.ChartHosts, tk.TasksFrom)
	}
	if tk.Name != "部署" {
		t.Fatalf("任务名被 map 内 name 覆盖: %q", tk.Name)
	}
}

func TestChartRefRejects(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want string
	}{
		{"vars+values 同现", map[string]any{"chart": map[string]any{"name": "a", "values": map[string]any{"k": 2}}, "vars": map[string]any{"k": 1}}, "cannot be used together"},
		{"phase+tasks_from 同现", map[string]any{"chart": map[string]any{"name": "a", "phase": "x"}, "tasks_from": "y"}, "cannot be used together"},
		{"map 未知键", map[string]any{"chart": map[string]any{"name": "a", "bogus": 1}}, "unknown chart reference key"},
		{"map 缺 name", map[string]any{"chart": map[string]any{"values": map[string]any{}}}, "requires a name"},
		{"混入模块键", map[string]any{"chart": map[string]any{"name": "a"}, "shell": "x"}, "cannot be combined"},
		{"map 内嵌 chart", map[string]any{"chart": map[string]any{"name": "a", "chart": "b"}}, "cannot nest"},
		{"map 内控制键错位", map[string]any{"chart": map[string]any{"name": "a", "when": "true"}}, "belongs at the task level"},
	}
	for _, c := range cases {
		if _, err := parseTask(c.m, false); err == nil {
			t.Errorf("%s: 应报错", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 错误信息应含 %q，实际 %v", c.name, c.want, err)
		}
	}
	// 普通模块任务携带 values 键：values 不在控制键白名单 → 当作第二模块报错
	if _, err := parseTask(map[string]any{"shell": "x", "values": map[string]any{"a": 1}}, false); err == nil {
		t.Error("普通任务的 values 键应被拒绝（会静默失效）")
	}
}

func TestCollectChartRefs(t *testing.T) {
	dir := t.TempDir()
	pb := filepath.Join(dir, "site.yaml")
	content := `
- hosts: all
  tasks:
    - chart: {name: jdk}
    - block:
        - chart: {name: nginx, hosts: web}
        - shell: uptime
      rescue:
        - chart: {name: jdk@^1.0}
- hosts: db
  handlers:
    - chart: {name: nginx}
`
	if err := os.WriteFile(pb, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	plays, err := Load(pb)
	if err != nil {
		t.Fatal(err)
	}
	refs := CollectChartRefs(plays)
	if len(refs) != 3 {
		t.Fatalf("应收集 3 个去重引用（jdk/nginx/jdk@^1.0 算两个写法）: %v", refs)
	}
	// 嵌套 block 内的引用被收集
	found := map[string]bool{}
	for _, r := range refs {
		found[r] = true
	}
	if !found["nginx"] || !found["jdk@^1.0"] {
		t.Fatalf("block/handler 内引用未收集: %v", refs)
	}
}

func TestChartRefLintResolvesChartsDir(t *testing.T) {
	dir := t.TempDir()
	pb := filepath.Join(dir, "site.yaml")
	if err := os.WriteFile(pb, []byte("- hosts: all\n  tasks:\n    - chart: {name: greet}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 无 charts/greet → ERROR 指明解析根
	issues := Lint(pb)
	if len(issues) != 1 || issues[0].Level != "ERROR" {
		t.Fatalf("缺 chart 目录应报 ERROR: %v", issues)
	}
	// 建 charts/greet 后通过
	if err := os.MkdirAll(filepath.Join(dir, "charts", "greet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if issues := Lint(pb); len(issues) != 0 {
		t.Fatalf("chart 目录存在时不应报错: %v", issues)
	}
}
