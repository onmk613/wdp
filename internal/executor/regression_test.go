package executor

// 审计修复回归测试：handler 按主机派发、when 跳过注册、空 loop results、
// 跨 play 失败主机隔离、until 重试语义、chart 环引用防护。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/conn/fake"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

// TestHandlerOnlyOnNotifyingHost 只有通知了 handler 的主机才执行该 handler
// （回归：此前 handler 扇出到全部存活主机，一台改配置会全集群重启服务）。
func TestHandlerOnlyOnNotifyingHost(t *testing.T) {
	var mu sync.Mutex
	handlerHosts := map[string]bool{}
	ex, rep := setup(t, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "handler-ran") {
			mu.Lock()
			handlerHosts[host] = true
			mu.Unlock()
		}
		return conn.ExecResult{Code: 0}, nil
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
	ex, rep := setup(t, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "downstream-ok") {
			mu.Lock()
			rendered[host] = true
			mu.Unlock()
		}
		return conn.ExecResult{Code: 0}, nil
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
	ex, rep := setup(t, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "zero-ok") {
			mu.Lock()
			rendered[host] = true
			mu.Unlock()
		}
		return conn.ExecResult{Code: 0}, nil
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
	ex, rep := setup(t, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "fail-h1") && host == "h1" {
			return conn.ExecResult{Code: 1, Stderr: "boom"}, nil
		}
		if strings.Contains(req.Script, "second-play") {
			mu.Lock()
			second[host] = true
			mu.Unlock()
		}
		return conn.ExecResult{Code: 0}, nil
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
	if !strings.Contains(rep.joined(), "failed previously") {
		t.Fatalf("应提示主机被跳过:\n%s", rep.joined())
	}
}

// TestUntilRetriesPlusOne until 的 retries 表示重试次数，总尝试 = retries+1
// （回归：此前 until 把 retries 当总次数，与无 until 的语义不一致）。
func TestUntilRetriesPlusOne(t *testing.T) {
	calls := 0
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		calls++
		return conn.ExecResult{Code: 1}, nil // 永不满足
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
	if !strings.Contains(rep.joined(), "unmet after 3 attempts") {
		t.Fatalf("%s", rep.joined())
	}
}

// TestChartSelfReferenceNoCrash chart 自引用不再栈溢出崩溃，而是清晰报错。
func TestChartSelfReferenceNoCrash(t *testing.T) {
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	conn.RegisterFactory("fake", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		return fake.NewFake(h), nil
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
	ex := New(inv, conn.NewManager(), rep, Options{Forks: 2, Chart: root, Values: map[string]any{}, BaseDir: t.TempDir()})
	if !ex.Run(context.Background(), root.Deploy) {
		t.Fatalf("环引用应报告失败（而非崩溃）:\n%s", rep.joined())
	}
	if !strings.Contains(rep.joined(), "depth limit") {
		t.Fatalf("应报告环引用深度错误:\n%s", rep.joined())
	}
}

// TestLastStatsIsSnapshot LastStats 返回快照：修改返回 map 不影响内部统计。
func TestLastStatsIsSnapshot(t *testing.T) {
	ex, rep := setup(t, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0}, nil
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

// TestLastStatsAccumulatesPlays LastStats 是跨 play 的累计统计（部署记录
// 用）：此前每个 play 整体替换 e.stats，多 play 部署的 release 记录只剩
// 最后一个 play 的统计。
func TestLastStatsAccumulatesPlays(t *testing.T) {
	ex, rep := setup(t, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{
		{Hosts: "webservers", Tasks: []*model.Task{{Name: "install", Module: "shell", FreeForm: "a"}}},
		{Hosts: "webservers", Tasks: []*model.Task{{Name: "start", Module: "shell", FreeForm: "b"}}},
	}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("%s", rep.joined())
	}
	stats := ex.LastStats()
	for _, h := range []string{"h1", "h2"} {
		if stats[h] == nil || stats[h].Changed != 2 {
			t.Fatalf("%s 应累计两个 play 的 changed=2: %+v", h, stats[h])
		}
	}
}

// TestAggregateLoopOutputCap loop 聚合输出有总量上限：单项输出各有 1MiB
// 截断，但聚合逐项 += 不封顶会在万级 item 下撑爆内存。触顶后总量恒不超
// 上限且带截断标记。
func TestAggregateLoopOutputCap(t *testing.T) {
	big := strings.Repeat("x", maxOutLen/2+128)
	res := &model.TaskResult{}
	loop := []*model.TaskResult{}
	for i := 0; i < 4; i++ { // 4 × ~512KiB = ~2MiB，必然触顶
		loop = append(loop, &model.TaskResult{Changed: true, Stdout: big})
	}
	if stop := aggregateLoopResults(res, loop); stop {
		t.Fatal("全部成功不应终止聚合")
	}
	if len(res.Stdout) > maxOutLen {
		t.Fatalf("聚合输出总量 %d 应 ≤ %d", len(res.Stdout), maxOutLen)
	}
	if !strings.Contains(res.Stdout, "aggregate output truncated") {
		t.Fatal("触顶后应带截断标记")
	}
	if !res.Changed {
		t.Fatal("聚合 changed 判定不应受截断影响")
	}
	// 未触顶时不加标记
	small := &model.TaskResult{}
	if stop := aggregateLoopResults(small, []*model.TaskResult{{Stdout: "abc"}, {Stdout: "def"}}); stop {
		t.Fatal("不应终止")
	}
	if small.Stdout != "abcdef" || strings.Contains(small.Stdout, "truncated") {
		t.Fatalf("未触顶聚合应原样拼接: %q", small.Stdout)
	}
}
