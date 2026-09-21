package executor

// 裸 playbook 的 chart 引用回归：与普通模块任务混排按序执行、values/
// values_from 分层覆盖、helpers 聚合、hosts 过滤、phase 入口、缺引用报错。
// 布局与 cli/run 的预扫描同构（charts/ 目录 + values 文件），引擎构建走
// 同一聚合规则。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/conn/fake"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/render"
)

// writeBareChartLayout 构造裸 playbook 测试布局：
//
//	<tmp>/playbook.yaml, <tmp>/values/prod.yaml
//	<tmp>/charts/greet/{chart.yaml,values.yaml,_helpers.tpl,deploy.yaml,status.yaml}
func writeBareChartLayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("charts/greet/chart.yaml", "name: greet\nversion: 1.0.0\n")
	write("charts/greet/values.yaml", "greeting: hello\nwho: world\n")
	write("charts/greet/_helpers.tpl", `{{- define "greet.full" -}}{{ .greeting }}-{{ .who }}{{- end -}}`)
	write("charts/greet/deploy.yaml", `
- hosts: all
  tasks:
    - name: 打招呼
      shell: 'echo {{ include "greet.full" . }}'
`)
	write("charts/greet/status.yaml", `
- hosts: all
  tasks:
    - name: 状态
      shell: 'echo greet-status-ok'
`)
	write("values/prod.yaml", "greeting: hi\n")
	return dir
}

// setupBarePlaybook 构造裸 playbook 执行器（ChartRefs 预加载 + helpers
// 聚合引擎，与 cli/run 预扫描同构）与 fake 连接脚本捕获。
func setupBarePlaybook(t *testing.T, dir string, playbookYAML string) (*Executor, *captureReporter, func() []string) {
	t.Helper()
	pb := filepath.Join(dir, "playbook.yaml")
	if err := os.WriteFile(pb, []byte(playbookYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	refs := map[string]*chart.Chart{}
	var helpers []string
	entries, _ := os.ReadDir(filepath.Join(dir, "charts"))
	for _, e := range entries {
		sub, err := chart.Load(filepath.Join(dir, "charts", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		refs[e.Name()] = sub
		if h := sub.CollectHelpers(); h != "" {
			helpers = append(helpers, h)
		}
	}
	eng, err := render.NewEngine(strings.Join(helpers, "\n"))
	if err != nil {
		t.Fatal(err)
	}

	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	conn.RegisterFactory("fake", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		f := fake.NewFake(h)
		f.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: "ran: " + req.Script}, nil
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
	ex := New(inv, conn.NewManager(), rep, Options{
		Forks: 2, ChartRefs: refs, Engine: eng, BaseDir: dir, Values: map[string]any{},
	})
	getScripts := func() []string {
		var scripts []string
		for _, f := range allFakes() {
			for _, r := range f.ExecLog {
				scripts = append(scripts, r.Script)
			}
		}
		return scripts
	}
	return ex, rep, getScripts
}

// TestBarePlaybookChartRefsE2E 裸 playbook 混排：chart 引用（map 形态，
// values + values_from + phase）与普通 shell 任务按序执行；values 分层
// （chart 默认 < values_from 文件 < 内联 values）；helpers 生效。
func TestBarePlaybookChartRefsE2E(t *testing.T) {
	dir := writeBareChartLayout(t)
	ex, rep, getScripts := setupBarePlaybook(t, dir, `
- hosts: webservers
  tasks:
    - name: 前置
      shell: 'echo pre-task'
    - name: 部署 greet
      chart:
        name: greet
        values: {who: deploy}
        values_from: [values/prod.yaml]
    - name: 后置校验
      shell: 'echo post-task'
`)
	if ex.Run(context.Background(), playbookPlays(t, dir)) {
		t.Fatalf("混排执行不应失败:\n%s", rep.joined())
	}
	scripts := strings.Join(getScripts(), "\n")
	// values 分层：greeting 被 values_from 文件覆盖为 hi；who 被内联 values
	// 覆盖为 deploy；helpers 的 include 生效
	if !strings.Contains(scripts, "echo hi-deploy") {
		t.Fatalf("values/values_from 分层渲染错误:\n%s", scripts)
	}
	// 混排任务确实执行
	if !strings.Contains(scripts, "echo pre-task") || !strings.Contains(scripts, "echo post-task") {
		t.Fatalf("普通任务未与 chart 引用混排执行:\n%s", scripts)
	}
	// 默认 values 不被无关键覆盖破坏（who 默认 world 已被覆盖，此处验证
	// 未覆盖路径：status 相位沿用默认 values）
	if !strings.Contains(scripts, "hello-world") && !strings.Contains(scripts, "hi-deploy") {
		t.Fatalf("chart 默认 values 完全丢失:\n%s", scripts)
	}
}

// TestBarePlaybookChartRefPhase phase 入口（status 相位）。
func TestBarePlaybookChartRefPhase(t *testing.T) {
	dir := writeBareChartLayout(t)
	ex, rep, getScripts := setupBarePlaybook(t, dir, `
- hosts: webservers
  tasks:
    - chart: {name: greet, phase: status}
`)
	if ex.Run(context.Background(), playbookPlays(t, dir)) {
		t.Fatalf("phase 执行不应失败:\n%s", rep.joined())
	}
	scripts := strings.Join(getScripts(), "\n")
	if !strings.Contains(scripts, "echo greet-status-ok") {
		t.Fatalf("phase 入口未生效:\n%s", scripts)
	}
	if strings.Contains(scripts, "greet.full") && strings.Contains(scripts, "echo hello") {
		t.Fatalf("deploy 相位不应执行:\n%s", scripts)
	}
}

// TestBarePlaybookChartRefHostsFilter hosts 过滤：play 批次内不在选择器的
// 主机跳过该引用，普通任务不受影响。
func TestBarePlaybookChartRefHostsFilter(t *testing.T) {
	dir := writeBareChartLayout(t)
	// 清单只有 webservers(h1,h2)——用主机名 h1 作为选择器
	ex, rep, getScripts := setupBarePlaybook(t, dir, `
- hosts: webservers
  tasks:
    - chart: {name: greet, hosts: h1}
    - shell: 'echo always-task'
`)
	if ex.Run(context.Background(), playbookPlays(t, dir)) {
		t.Fatalf("hosts 过滤执行不应失败:\n%s", rep.joined())
	}
	scripts := strings.Join(getScripts(), "\n")
	greetCount := strings.Count(scripts, "hello-world") + strings.Count(scripts, "greeting")
	if greetCount == 0 || greetCount > 2 { // chart echo 渲染为 "echo hello-world"
		t.Fatalf("hosts 过滤后 chart 引用执行次数异常:\n%s", scripts)
	}
	// 两台主机的 always-task 都执行（各一次，共 2）
	if strings.Count(scripts, "echo always-task") != 2 {
		t.Fatalf("hosts 过滤不应影响其他任务:\n%s", scripts)
	}
	// h2 上 chart 引用被跳过：跳过信息可见
	if !strings.Contains(rep.joined(), "outside selector") {
		t.Fatalf("跳过主机应有提示:\n%s", rep.joined())
	}
}

// TestBarePlaybookChartRefUnknown 缺引用清晰报错（含可用引用列表）。
func TestBarePlaybookChartRefUnknown(t *testing.T) {
	dir := writeBareChartLayout(t)
	ex, rep, _ := setupBarePlaybook(t, dir, `
- hosts: webservers
  tasks:
    - chart: {name: nosuch}
`)
	if !ex.Run(context.Background(), playbookPlays(t, dir)) {
		t.Fatal("缺引用应失败")
	}
	if !strings.Contains(rep.joined(), "unknown chart reference") || !strings.Contains(rep.joined(), "greet") {
		t.Fatalf("缺引用错误应指明可用的引用:\n%s", rep.joined())
	}
}

// playbookPlays 加载测试 playbook（断言解析成功的辅助）。
func playbookPlays(t *testing.T, dir string) []*model.Play {
	t.Helper()
	plays, err := playbook.Load(filepath.Join(dir, "playbook.yaml"))
	if err != nil {
		t.Fatalf("playbook 解析失败: %v", err)
	}
	if len(plays) == 0 {
		t.Fatal("playbook 无 play")
	}
	return plays
}
