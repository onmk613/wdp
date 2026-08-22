package executor

// 审计修复回归测试：handler 按主机派发、when 跳过注册、空 loop results、
// 跨 play 失败主机隔离、until 重试语义、chart 环引用防护。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

// TestHandlerOnlyOnNotifyingHost 只有通知了 handler 的主机才执行该 handler
// （回归：此前 handler 扇出到全部存活主机，一台改配置会全集群重启服务）。
func TestHandlerOnlyOnNotifyingHost(t *testing.T) {
	var mu sync.Mutex
	handlerHosts := map[string]bool{}
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "handler-ran") {
			mu.Lock()
			handlerHosts[host] = true
			mu.Unlock()
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{{
			Name:        "只改 h1",
			Module:      "shell",
			FreeForm:    "touch",
			ChangedWhen: `{{ eq .inventory_hostname "h1" }}`,
			Notify:      []string{"restart"},
		}},
		Handlers: []*model.Task{{Name: "restart", Module: "shell", FreeForm: "handler-ran"}},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("%s", rep.joined())
	}
	if !handlerHosts["h1"] {
		t.Fatalf("h1 变更后 handler 应在 h1 执行:\n%s", rep.joined())
	}
	if handlerHosts["h2"] {
		t.Fatalf("h2 未变更，handler 不应在 h2 执行:\n%s", rep.joined())
	}
}

// TestWhenSkipRegisters 跳过的任务同样 register（skipped=true），
// 使 `when: not r.skipped` 这类惯用法可用。
func TestWhenSkipRegisters(t *testing.T) {
	var mu sync.Mutex
	rendered := map[string]bool{}
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "downstream-ok") {
			mu.Lock()
			rendered[host] = true
			mu.Unlock()
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "跳过", Module: "shell", FreeForm: "never", When: []string{"false"}, Register: "r"},
			{Name: "引用", Module: "shell", FreeForm: `echo {{ if .r.skipped }}downstream-ok{{ end }}`},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("%s", rep.joined())
	}
	if !rendered["h1"] || !rendered["h2"] {
		t.Fatalf("r.skipped 应可被下游引用:\n%s", rep.joined())
	}
}

// TestEmptyLoopRegistersResults 空 loop 注册 results=[]，
// 下游 len .r.results 不报 map has no entry。
func TestEmptyLoopRegistersResults(t *testing.T) {
	var mu sync.Mutex
	rendered := map[string]bool{}
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "zero-ok") {
			mu.Lock()
			rendered[host] = true
			mu.Unlock()
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "空循环", Module: "shell", FreeForm: "x", Loop: []any{}, Register: "lr"},
			{Name: "引用", Module: "shell", FreeForm: `echo {{ if eq (len .lr.results) 0 }}zero-ok{{ end }}`},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("%s", rep.joined())
	}
	if !rendered["h1"] || !rendered["h2"] {
		t.Fatalf("空 loop 应注册空 results:\n%s", rep.joined())
	}
}

// TestFailedHostSkippedInLaterPlays play1 失败的主机不再参与后续 play
// （回归：此前失败主机继续执行后续 play，install 失败仍会 start service）。
func TestFailedHostSkippedInLaterPlays(t *testing.T) {
	var mu sync.Mutex
	second := map[string]bool{}
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "fail-h1") && host == "h1" {
			return connection.ExecResult{Code: 1, Stderr: "boom"}, nil
		}
		if strings.Contains(req.Script, "second-play") {
			mu.Lock()
			second[host] = true
			mu.Unlock()
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{
		{Hosts: "webservers", Tasks: []*model.Task{{Name: "install", Module: "shell", FreeForm: "fail-h1"}}},
		{Hosts: "webservers", Tasks: []*model.Task{{Name: "start", Module: "shell", FreeForm: "second-play"}}},
	}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("play1 失败应报告失败")
	}
	if second["h1"] {
		t.Fatalf("h1 在 play1 失败，不应执行 play2:\n%s", rep.joined())
	}
	if !second["h2"] {
		t.Fatalf("h2 正常，应执行 play2:\n%s", rep.joined())
	}
	if !strings.Contains(rep.joined(), "此前失败") {
		t.Fatalf("应提示主机被跳过:\n%s", rep.joined())
	}
}

// TestUntilRetriesPlusOne until 的 retries 表示重试次数，总尝试 = retries+1
// （回归：此前 until 把 retries 当总次数，与无 until 的语义不一致）。
func TestUntilRetriesPlusOne(t *testing.T) {
	calls := 0
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		calls++
		return connection.ExecResult{Code: 1}, nil // 永不满足
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{{
			Name: "等待", Module: "shell", FreeForm: "x",
			Until:   `{{ if eq .result.rc 0 }}ok{{ end }}`,
			Retries: 2, DelaySec: 0,
		}},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("until 耗尽应失败")
	}
	if calls != 3 {
		t.Fatalf("retries=2 应为 3 次尝试（1+2），实际 %d", calls)
	}
	if !strings.Contains(rep.joined(), "3 次尝试后仍未满足") {
		t.Fatalf("%s", rep.joined())
	}
}

// TestChartSelfReferenceNoCrash chart 自引用不再栈溢出崩溃，而是清晰报错。
func TestChartSelfReferenceNoCrash(t *testing.T) {
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	connection.RegisterFactory("fake", func(h *model.Host, dc *connection.Defaults) (connection.Connection, error) {
		return connection.NewFake(h), nil
	})
	inv, err := inventory.Parse([]byte(testInv))
	if err != nil {
		t.Fatal(err)
	}
	rep := &captureReporter{}

	// b 的 deploy 引用 b 自身：runChartTask(b) → runTaskOnHost → runChartTask(b) → …无限下钻
	root := &chart.Chart{
		Meta:   chart.Meta{Name: "a", Version: "1.0.0"},
		Values: map[string]any{},
		Subs: map[string]*chart.Chart{
			"b": {
				Meta:   chart.Meta{Name: "b", Version: "1.0.0"},
				Values: map[string]any{},
				Deploy: []*model.Play{{Hosts: "webservers", Tasks: []*model.Task{
					{Name: "自引用", ChartRef: "b"},
				}}},
			},
		},
		Deploy: []*model.Play{{Hosts: "webservers", Tasks: []*model.Task{
			{Name: "入口", ChartRef: "b"},
		}}},
	}
	ex := New(inv, connection.NewManager(), rep, Options{Forks: 2, Chart: root, Values: map[string]any{}, BaseDir: t.TempDir()})
	if !ex.Run(context.Background(), root.Deploy) {
		t.Fatalf("环引用应报告失败（而非崩溃）:\n%s", rep.joined())
	}
	if !strings.Contains(rep.joined(), "深度上限") {
		t.Fatalf("应报告环引用深度错误:\n%s", rep.joined())
	}
}

// TestLastStatsIsSnapshot LastStats 返回快照：修改返回 map 不影响内部统计。
func TestLastStatsIsSnapshot(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{{Name: "ok", Module: "shell", FreeForm: "true"}},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("%s", rep.joined())
	}
	snap := ex.LastStats()
	snap["h1"].Changed = 999 // 篡改快照
	again := ex.LastStats()
	if again["h1"].Changed != 1 {
		t.Fatalf("快照应隔离内部状态: h1.Changed=%d", again["h1"].Changed)
	}
}
