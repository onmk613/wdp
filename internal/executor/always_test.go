package executor

// 回归：block 主机不可达时 always 仍被尝试（连接故障时清理任务不应被静默跳过）。

import (
	"context"
	"errors"
	"strings"

	"testing"

	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

// TestAlwaysRunsOnUnreachable block 不可达时 always 任务仍被尝试
// （此前直接跳过，清理契约被反转）。
func TestAlwaysRunsOnUnreachable(t *testing.T) {
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	// 连接建立即失败 → 所有任务 unreachable
	connection.RegisterFactory("fake", func(h *model.Host, dc *connection.Defaults) (connection.Connection, error) {
		f := connection.NewFake(h)
		f.ConnectErr = errors.New("connection failed")
		return f, nil
	})
	inv, err := inventory.Parse([]byte(testInv))
	if err != nil {
		t.Fatal(err)
	}
	rep := &captureReporter{}
	ex := New(inv, connection.NewManager(), rep, Options{Forks: 2})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{{
			Name: "组",
			Block: []*model.Task{
				{Name: "blk", Module: "shell", FreeForm: "x"},
			},
			Always: []*model.Task{
				{Name: "清理", Module: "shell", FreeForm: "cleanup"},
			},
			IgnoreErrors: true,
		}},
	}}
	_ = ex.Run(context.Background(), plays)
	out := rep.joined()
	// block 任务与 always 任务各记一次 unreachable：每条主机消息里应出现两次连接失败
	// （第二次即 always 清理任务的尝试记录，此前会被整体跳过）
	for _, h := range []string{"h1", "h2"} {
		if n := strings.Count(out, "主机 "+h+" 连接失败"); n < 2 {
			t.Fatalf("%s 的 always 应被尝试并记录 unreachable（期望 ≥2 次连接失败记录）:\n%s", h, out)
		}
	}
}
