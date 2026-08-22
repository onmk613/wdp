package executor

// 大规模主机基准：1000 台 fake 主机 × 5 个 shell 任务。
// fake 连接零网络开销，度量的是执行器自身的并发调度/变量域/统计开销
// （README 的 "1000 台 × 5 任务实测约 5 秒" 以真实 SSH 为准；本基准
// 提供执行器层的可回归参照：应稳定在数百毫秒量级）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

func BenchmarkRun1000Hosts5Tasks(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("bench:\n  hosts:\n")
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&sb, "    h%03d: {conn: fake}\n", i)
	}
	invSrc := []byte(sb.String())

	connection.RegisterFactory("fake", func(h *model.Host, dc *connection.Defaults) (connection.Connection, error) {
		f := connection.NewFake(h)
		f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
			return connection.ExecResult{Code: 0, Stdout: "ok\n"}, nil
		}
		return f, nil
	})
	inv, err := inventory.Parse(invSrc)
	if err != nil {
		b.Fatal(err)
	}
	tasks := make([]*model.Task, 5)
	for i := range tasks {
		tasks[i] = &model.Task{Name: fmt.Sprintf("t%d", i), Module: "shell", FreeForm: "true"}
	}
	play := &model.Play{Name: "bench", Hosts: "bench", Tasks: tasks}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ex := New(inv, connection.NewManager(), nopReporter{}, Options{Forks: 20})
		if failed := ex.Run(context.Background(), []*model.Play{play}); failed {
			b.Fatal("unexpected failure")
		}
	}
}

// nopReporter 静默 reporter（基准不度量输出渲染）。
type nopReporter struct{}

func (nopReporter) PlayStart(string, []string)            {}
func (nopReporter) TaskStart(string, string)              {}
func (nopReporter) HostResult(string, *model.TaskResult)  {}
func (nopReporter) TaskDone()                             {}
func (nopReporter) PlayMsg(string, ...any)                {}
func (nopReporter) Recap(string, map[string]*model.Stats) {}
func (nopReporter) Finish()                               {}
