package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
)

// captureReporter 记录全部事件供断言。
type captureReporter struct {
	mu     sync.Mutex
	lines  []string
	recaps map[string]*model.Stats
}

func (c *captureReporter) PlayStart(name string, hosts []string) {
	c.emit(fmt.Sprintf("PLAY %s hosts=%v", name, hosts))
}

func (c *captureReporter) TaskStart(task, module string) {
	c.emit(fmt.Sprintf("TASK %s (%s)", task, module))
}

func (c *captureReporter) HostResult(host string, r *model.TaskResult) {
	c.emit(fmt.Sprintf("%s %s failed=%v changed=%v skipped=%v unreachable=%v msg=%s",
		host, r.Task, r.Failed, r.Changed, r.Skipped, r.Unreachable, r.Msg))
}

func (c *captureReporter) PlayMsg(format string, a ...any) {
	c.emit("MSG " + fmt.Sprintf(format, a...))
}

// TaskDone 记录任务收尾事件。
func (c *captureReporter) TaskDone() {}

// Finish 记录 run 收尾事件。
func (c *captureReporter) Finish() {}

func (c *captureReporter) Recap(playName string, stats map[string]*model.Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recaps = stats
}

func (c *captureReporter) emit(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, s)
}

func (c *captureReporter) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

const testInv = `
webservers:
  hosts:
    h1: {conn: fake}
    h2: {conn: fake}
`

var (
	fakeMu sync.Mutex
	fakes  []*connection.Fake
)

// setup 注册 fake 连接工厂（并记录实例）并构造执行器。
// script 可按主机与请求内容返回不同结果。
func setup(t *testing.T, script func(host string, req connection.ExecRequest) (connection.ExecResult, error)) (*Executor, *captureReporter) {
	t.Helper()
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	connection.RegisterFactory("fake", func(h *model.Host) (connection.Connection, error) {
		f := connection.NewFake(h)
		f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
			return script(h.Name, req)
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
	ex := New(inv, connection.NewManager(), rep, Options{Forks: 2})
	return ex, rep
}

func allFakes() []*connection.Fake {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	return append([]*connection.Fake{}, fakes...)
}

func okExec(string, connection.ExecRequest) (connection.ExecResult, error) {
	return connection.ExecResult{Code: 0, Stdout: "done\n"}, nil
}

func TestBasicRun(t *testing.T) {
	ex, rep := setup(t, okExec)
	plays := []*model.Play{{
		Name: "basic", Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "uptime", Module: "shell", FreeForm: "uptime"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	out := rep.joined()
	if !strings.Contains(out, "TASK uptime (shell)") {
		t.Fatalf("%s", out)
	}
	if rep.recaps["h1"].Changed != 1 || rep.recaps["h2"].Changed != 1 {
		t.Fatalf("recap: %+v", rep.recaps)
	}
}

func TestWhenAndRegister(t *testing.T) {
	ex, rep := setup(t, okExec)
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "probe", Module: "shell", FreeForm: "check", Register: "probe"},
			{Name: "conditional", Module: "shell", FreeForm: "go", When: []string{`{{ if not .probe.failed }}go{{ end }}`}},
			{Name: "never", Module: "shell", FreeForm: "skip", When: []string{"false"}},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	out := rep.joined()
	// probe rc=0 → 条件真 → 执行
	if !strings.Contains(out, "h1 conditional failed=false changed=true") {
		t.Fatalf("条件任务应执行:\n%s", out)
	}
	if !strings.Contains(out, "skipped=true") {
		t.Fatalf("false 条件应跳过:\n%s", out)
	}
}

func TestHandlerNotify(t *testing.T) {
	ex, rep := setup(t, okExec)
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "change-something", Module: "shell", FreeForm: "touch", Notify: []string{"restart"}},
		},
		Handlers: []*model.Task{
			{Name: "restart", Module: "service", Args: map[string]any{"name": "nginx", "state": "restarted"}},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	if !strings.Contains(rep.joined(), "restart (handler)") {
		t.Fatalf("handler 未触发:\n%s", rep.joined())
	}
}

func TestFailureIsolation(t *testing.T) {
	script := func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if host == "h1" {
			return connection.ExecResult{Code: 1, Stderr: "boom"}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	}
	ex, rep := setup(t, script)
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "fail-task", Module: "shell", FreeForm: "false"},
			{Name: "after", Module: "shell", FreeForm: "uptime"},
		},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("存在失败应返回 true")
	}
	out := rep.joined()
	if strings.Contains(out, "h1 after") {
		t.Fatalf("失败主机不应执行后续任务:\n%s", out)
	}
	if !strings.Contains(out, "h2 after") {
		t.Fatalf("健康主机应继续:\n%s", out)
	}
	if rep.recaps["h1"].Failed != 1 {
		t.Fatalf("recap h1: %+v", rep.recaps["h1"])
	}
}

func TestIgnoreErrors(t *testing.T) {
	script := func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if req.Script == "x" {
			return connection.ExecResult{Code: 1}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	}
	ex, rep := setup(t, script)
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Name: "tolerated", Module: "shell", FreeForm: "x", IgnoreErrors: true},
			{Name: "after", Module: "shell", FreeForm: "y"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("ignore_errors 不应判定 play 失败:\n%s", rep.joined())
	}
	if rep.recaps["h1"].Ignored != 1 {
		t.Fatalf("recap: %+v", rep.recaps["h1"])
	}
	if !strings.Contains(rep.joined(), "h1 after") {
		t.Fatalf("ignore 后应继续:\n%s", rep.joined())
	}
}

func TestLoopItems(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0, Stdout: "ok " + req.Script}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Name: "loop", Module: "shell", FreeForm: "echo {{ .item }}", Loop: []any{"a", "b", "c"}},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	fs := allFakes()
	if len(fs) != 1 || len(fs[0].ExecLog) != 3 {
		t.Fatalf("应执行 3 次，实际 fakes=%d execs=%d", len(fs), len(fs[0].ExecLog))
	}
	joined := ""
	for _, r := range fs[0].ExecLog {
		joined += r.Script + "|"
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %s: %s", want, joined)
		}
	}
}

// TestLoopTemplateList 单模板元素渲染结果为 JSON 列表字符串时展开为多项；
// 非列表语法的渲染结果保持单元素语义。
func TestLoopTemplateList(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			// to_json 输出 ["p","q"] → 展开为 2 项
			{Name: "json", Module: "shell", FreeForm: "echo {{ .item }}", Loop: []any{`{{ to_json (list "p" "q") }}`}},
			// 普通模板输出单字符串 → 单项
			{Name: "scalar", Module: "shell", FreeForm: "echo {{ .item }}", Loop: []any{`{{ printf "plain" }}`}},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	joined := ""
	for _, r := range allFakes()[0].ExecLog {
		joined += r.Script + "|"
	}
	if !strings.Contains(joined, "echo p|") || !strings.Contains(joined, "echo q|") {
		t.Fatalf("模板列表应展开为 p/q 两项: %s", joined)
	}
	if !strings.Contains(joined, "echo plain") {
		t.Fatalf("普通模板应保持单项: %s", joined)
	}
}

func TestUnknownModule(t *testing.T) {
	ex, rep := setup(t, okExec)
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{{Module: "nonexistent", FreeForm: "x"}},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("未知模块应失败")
	}
	if !strings.Contains(rep.joined(), "未知模块") {
		t.Fatalf("%s", rep.joined())
	}
}

func TestSetupFactsIntoVars(t *testing.T) {
	factsOut := "hostname=test-host\nkernel=6.1.0\narch=x86_64\nos_id=ubuntu\nos_name=Ubuntu\n" +
		"os_version=22.04\ndefault_ipv4=10.0.0.5\ncpus=4\nmemory_mb=2048\n" +
		"disk_total=100\ndisk_used=50\ndisk_avail=50\ndisk_pct=50%\n"
	script := func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "/etc/os-release") || strings.Contains(req.Script, "df -B1") {
			return connection.ExecResult{Code: 0, Stdout: factsOut}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	}
	ex, rep := setup(t, script)
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Module: "setup"},
			{Name: "use-facts", Module: "shell", FreeForm: "echo {{ .os.family }}/{{ .cpus }}"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	// use-facts 的 free-form 应完成变量替换
	fs := allFakes()
	var last string
	for _, r := range fs[0].ExecLog {
		last = r.Script
	}
	if !strings.Contains(last, "echo debian/4") {
		t.Fatalf("facts 未生效，最后脚本: %q", last)
	}
}

// TestModulesRegistered 确保全部内置模块就位。
func TestModulesRegistered(t *testing.T) {
	for _, name := range []string{"shell", "command", "script", "copy", "fetch", "file", "template", "package", "service", "setup"} {
		if _, ok := module.Get(name); !ok {
			t.Fatalf("缺少模块 %s", name)
		}
	}
}
