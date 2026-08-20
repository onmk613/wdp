package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/render"
)

// writeTestChart 写出用于 executor 测试的 chart（父 myapp + 子 jdk）。
func writeTestChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("chart.yaml", "name: myapp\nversion: 0.1.0\n")
	mustWrite("values.yaml", `
app:
  name: demo
  port: 8080
global:
  env: prod
jdk:
  version: "17"
`)
	mustWrite("_helpers.tpl", `{{ define "app.fullname" }}{{ .app.name }}-{{ .global.env }}{{ end }}`)
	mustWrite("deploy.yaml", `
- name: 主部署
  hosts: webservers
  tasks:
    - name: 安装 JDK
      chart: jdk
      vars:
        extra_opt: -Xmx2g
    - name: 引用结果
      shell: 'echo {{ .jdk_res.changed }}'
      register: outer
`)
	// 修正：register 应挂在 chart 任务上
	mustWrite("deploy.yaml", `
- name: 主部署
  hosts: webservers
  tasks:
    - name: 安装 JDK
      chart: jdk
      register: jdk_res
      vars:
        extra_opt: -Xmx2g
    - name: 引用 chart 结果
      shell: 'echo {{ .jdk_res.changed }}'
`)
	mustWrite("charts/jdk/chart.yaml", "name: jdk\nversion: 1.0.0\n")
	mustWrite("charts/jdk/values.yaml", "version: \"11\"\nhome: /opt/jdk\n")
	mustWrite("charts/jdk/deploy.yaml", `
- hosts: all
  tasks:
    - name: 子任务-版本与注入
      shell: 'echo v={{ .version }} opt={{ .extra_opt }} env={{ .global.env }}'
    - name: 子任务-register
      shell: 'echo home={{ .home }}'
      register: sub_reg
    - name: 子任务-引用同级 register
      shell: 'echo reg={{ .sub_reg.changed }}'
`)
	return dir
}

// setupChart 构建 chart 模式执行器 + fake 连接。
func setupChart(t *testing.T, extraValues map[string]any, sets []string) (*Executor, *captureReporter, func() []string) {
	return setupChartWith(t, nil, extraValues, sets)
}

// setupChartWith 同 setupChart，但可在加载前修改 chart 目录内容。
func setupChartWith(t *testing.T, mutate func(dir string), extraValues map[string]any, sets []string) (*Executor, *captureReporter, func() []string) {
	t.Helper()
	dir := writeTestChart(t)
	if mutate != nil {
		mutate(dir)
	}
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	ch, err := chart.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	values, err := ch.BuildValues(nil, sets)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range extraValues {
		values[k] = v
	}
	eng, err := render.NewEngine(ch.CollectHelpers())
	if err != nil {
		t.Fatal(err)
	}

	connection.RegisterFactory("fake", func(h *model.Host) (connection.Connection, error) {
		f := connection.NewFake(h)
		f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
			return connection.ExecResult{Code: 0, Stdout: "ran: " + req.Script}, nil
		}
		fakeMu.Lock()
		fakes = append(fakes, f)
		fakeMu.Unlock()
		return f, nil
	})
	inv, err := inventory.Parse([]byte(testInv))
	if err != nil {
		t.Fatal(err)
	}
	rep := &captureReporter{}
	ex := New(inv, connection.NewManager(), rep, Options{
		Forks: 2, Chart: ch, Values: values, Engine: eng, BaseDir: ch.Dir,
	})
	getScripts := func() []string {
		fakes := allFakes()
		var scripts []string
		for _, f := range fakes {
			for _, r := range f.ExecLog {
				scripts = append(scripts, r.Script)
			}
		}
		return scripts
	}
	return ex, rep, getScripts
}

func TestChartTaskScoping(t *testing.T) {
	ex, rep, getScripts := setupChart(t, nil, nil)
	if ex.Run(context.Background(), ex.Opts.Chart.Deploy) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	scripts := strings.Join(getScripts(), "\n---\n")

	// 子 chart 作用域：version 取父 values 的 jdk 子树（17，覆盖子默认 11）；
	// extra_opt 来自 chart 任务 vars 注入；global 可见
	if !strings.Contains(scripts, "echo v=17 opt=-Xmx2g env=prod") {
		t.Fatalf("子 chart 作用域渲染错误:\n%s", scripts)
	}
	// 子 chart 默认 values 中未被覆盖的键仍可见
	if !strings.Contains(scripts, "echo home=/opt/jdk") {
		t.Fatalf("子 chart 默认 values 丢失:\n%s", scripts)
	}
	// 子任务 register 在子序列内可见
	if !strings.Contains(scripts, "echo reg=true") {
		t.Fatalf("子 chart 内 register 失效:\n%s", scripts)
	}
	// 父任务可引用 chart 任务的聚合 register
	if !strings.Contains(scripts, "echo true") {
		t.Fatalf("父引用 chart register 失效:\n%s", scripts)
	}
}

func TestChartScopeIsolation(t *testing.T) {
	// 子 chart 引用父作用域的兄弟键（app.port）应渲染失败 —— 作用域隔离
	dir := writeTestChart(t)
	subDeploy := filepath.Join(dir, "charts", "jdk", "deploy.yaml")
	if err := os.WriteFile(subDeploy, []byte(`
- hosts: all
  tasks:
    - name: 越界引用
      shell: 'echo {{ .app.port }}'
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := chart.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	values, _ := ch.BuildValues(nil, nil)
	eng, _ := render.NewEngine(ch.CollectHelpers())

	connection.RegisterFactory("fake", func(h *model.Host) (connection.Connection, error) {
		return connection.NewFake(h), nil
	})
	inv, _ := inventory.Parse([]byte(testInv))
	rep := &captureReporter{}
	ex := New(inv, connection.NewManager(), rep, Options{
		Forks: 2, Chart: ch, Values: values, Engine: eng, BaseDir: ch.Dir,
	})
	if !ex.Run(context.Background(), ch.Deploy) {
		t.Fatal("越界引用应导致失败")
	}
	if !strings.Contains(rep.joined(), "app") || !strings.Contains(rep.joined(), "failed=true") {
		t.Fatalf("期望作用域隔离报错:\n%s", rep.joined())
	}
}

func TestChartSetOverride(t *testing.T) {
	// --set jdk.version=21 覆盖父 values 的子树值
	ex, rep, getScripts := setupChart(t, nil, []string{"jdk.version=21"})
	if ex.Run(context.Background(), ex.Opts.Chart.Deploy) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	if !strings.Contains(strings.Join(getScripts(), "\n"), "echo v=21") {
		t.Fatalf("--set 未穿透子 chart:\n%s", strings.Join(getScripts(), "\n"))
	}
}

func TestChartHelpersInParentTasks(t *testing.T) {
	// 父 chart 任务模板参数可引用 helpers 命名模板
	ex, rep, getScripts := setupChartWith(t, func(dir string) {
		deploy := filepath.Join(dir, "deploy.yaml")
		if err := os.WriteFile(deploy, []byte(`
- name: 主部署
  hosts: webservers
  tasks:
    - name: 使用 helpers
      shell: 'echo {{ include "app.fullname" . }}'
`), 0o644); err != nil {
			t.Fatal(err)
		}
	}, nil, nil)
	if ex.Run(context.Background(), ex.Opts.Chart.Deploy) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	scripts := strings.Join(getScripts(), "\n")
	if !strings.Contains(scripts, "echo demo-prod") {
		t.Fatalf("helpers 渲染错误:\n%s", scripts)
	}
}

func TestChartUnknownRef(t *testing.T) {
	ex, rep, _ := setupChart(t, nil, nil)
	// 篡改引用为不存在的子 chart
	ex.Opts.Chart.Deploy[0].Tasks[0].ChartRef = "nope"
	if !ex.Run(context.Background(), ex.Opts.Chart.Deploy) {
		t.Fatal("未知子 chart 引用应失败")
	}
	if !strings.Contains(rep.joined(), "子 chart") {
		t.Fatalf("%s", rep.joined())
	}
}

var _ = fmt.Sprintf // 保留 fmt 引用（如删除请一并移除 import）
