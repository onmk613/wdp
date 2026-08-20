package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

// setupFeature 构造指定 CheckMode 的执行器（fake 记入全局列表供断言）。
func setupFeature(t *testing.T, check bool, script func(string, connection.ExecRequest) (connection.ExecResult, error)) (*Executor, *captureReporter) {
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
	ex := New(inv, connection.NewManager(), rep, Options{Forks: 2, CheckMode: check})
	return ex, rep
}

func TestBlockRescue(t *testing.T) {
	// 主任务失败 → rescue 执行且组恢复；always 恒执行；后续任务继续
	var execCount int
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		execCount++
		if strings.Contains(req.Script, "will-fail") {
			return connection.ExecResult{Code: 1, Stderr: "boom"}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "容错组",
				Block: []*model.Task{
					{Name: "主操作", Module: "shell", FreeForm: "will-fail"},
				},
				Rescue: []*model.Task{
					{Name: "恢复", Module: "shell", FreeForm: "rescue-step"},
				},
				Always: []*model.Task{
					{Name: "总是", Module: "shell", FreeForm: "always-step"},
				},
			},
			{Name: "后续", Module: "shell", FreeForm: "after"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("rescue 成功应视为组恢复:\n%s", rep.joined())
	}
	out := rep.joined()
	if !strings.Contains(out, "已由 rescue 恢复") {
		t.Fatalf("缺少恢复信息:\n%s", out)
	}
	if !strings.Contains(out, "h1 后续") {
		t.Fatalf("组恢复后应继续执行:\n%s", out)
	}
	// block(1) + rescue(1) + always(1) + 后续(1)
	if execCount != 4 {
		t.Fatalf("执行次数 %d", execCount)
	}
}

// TestBlockIgnoreErrorsNotRescued block 内 ignore_errors 的失败不视为 block 失败：
// 后续任务继续、rescue 不触发。
func TestBlockIgnoreErrorsNotRescued(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "will-fail") {
			return connection.ExecResult{Code: 1}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "容错组",
				Block: []*model.Task{
					{Name: "可忽略失败", Module: "shell", FreeForm: "will-fail", IgnoreErrors: true},
					{Name: "紧随任务", Module: "shell", FreeForm: "next-step"},
				},
				Rescue: []*model.Task{
					{Name: "救援", Module: "shell", FreeForm: "rescue-step"},
				},
			},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("ignore_errors 的失败不应导致 play 失败:\n%s", rep.joined())
	}
	out := rep.joined()
	if strings.Contains(out, "rescue-step") || strings.Contains(out, "已由 rescue 恢复") {
		t.Fatalf("ignore_errors 失败不应触发 rescue:\n%s", out)
	}
	joined := ""
	for _, f := range allFakes() {
		for _, r := range f.ExecLog {
			joined += r.Script + "|"
		}
	}
	if !strings.Contains(joined, "next-step") {
		t.Fatalf("被忽略失败后的任务应继续执行: %s", joined)
	}
	if strings.Contains(joined, "rescue-step") {
		t.Fatalf("rescue 不应执行: %s", joined)
	}
}

func TestBlockNoRescueFails(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "will-fail") {
			return connection.ExecResult{Code: 1}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name:   "无救援组",
				Block:  []*model.Task{{Name: "主操作", Module: "shell", FreeForm: "will-fail"}},
				Always: []*model.Task{{Name: "总是", Module: "shell", FreeForm: "always-step"}},
			},
		},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("无 rescue 的 block 失败应判定失败")
	}
	out := rep.joined()
	if !strings.Contains(out, "failed=true") {
		t.Fatalf("%s", out)
	}
}

func TestUntilPolling(t *testing.T) {
	// 第 1、2 次返回非零，第 3 次成功；until 引用 .result.rc
	calls := 0
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		calls++
		if calls < 3 {
			return connection.ExecResult{Code: 1, Stderr: "not ready"}, nil
		}
		return connection.ExecResult{Code: 0, Stdout: "ready"}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "等待就绪", Module: "shell", FreeForm: "check-svc",
				Until:   `{{ if eq .result.rc 0 }}ok{{ end }}`,
				Retries: 5, DelaySec: 0,
			},
		},
	}}
	start := time.Now()
	if ex.Run(context.Background(), plays) {
		t.Fatalf("until 应在第 3 次满足:\n%s", rep.joined())
	}
	if calls != 3 {
		t.Fatalf("应执行 3 次，实际 %d", calls)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("delay=0 不应等待")
	}
}

func TestUntilExhausted(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 1}, nil // 永不满足
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "等待", Module: "shell", FreeForm: "x",
				Until:   `{{ if eq .result.rc 0 }}ok{{ end }}`,
				Retries: 2, DelaySec: 0,
			},
		},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("until 耗尽应失败")
	}
	if !strings.Contains(rep.joined(), "仍未满足") {
		t.Fatalf("%s", rep.joined())
	}
}

func TestCheckModeNoExecution(t *testing.T) {
	executed := 0
	ex, rep := setupFeature(t, true, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		executed++
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Name: "执行类", Module: "shell", FreeForm: "echo hi"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("check 模式不应失败:\n%s", rep.joined())
	}
	if executed != 0 {
		t.Fatalf("check 模式不应实际执行 shell（探测性脚本除外），实际执行 %d 次", executed)
	}
	if !strings.Contains(rep.joined(), "[check]") {
		t.Fatalf("%s", rep.joined())
	}
}

func TestTaskTimeout(t *testing.T) {
	// 任务 timeout=1s，脚本 sleep 5 → 超时失败
	ex, _ := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		time.Sleep(5 * time.Second)
		return connection.ExecResult{Code: 0}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Tasks: []*model.Task{
			{Name: "慢任务", Module: "shell", FreeForm: "sleep", TimeoutSec: 1},
		},
	}}
	start := time.Now()
	if !ex.Run(context.Background(), plays) {
		t.Fatal("超时应失败")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("未按任务超时中断")
	}
}
