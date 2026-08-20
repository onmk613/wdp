package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/report"
)

// TestNoLogRedactedInJSONReport 复现安全评审发现的泄漏路径：
// no_log 任务在 --output json（CI 场景）下 stdout 必须遮蔽，
// 普通任务输出不受影响。
func TestNoLogRedactedInJSONReport(t *testing.T) {
	ex, _ := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "rotate.sh") {
			return connection.ExecResult{Code: 0, Stdout: "SECRET-" + host + "\n"}, nil
		}
		return connection.ExecResult{Code: 0, Stdout: "done\n"}, nil
	})
	var buf bytes.Buffer
	jsonRep := report.NewJSONReporter(&buf)
	ex.Rep = jsonRep

	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "rotate-keys", Module: "shell", FreeForm: "rotate.sh", NoLog: true, Register: "out"},
			{Name: "normal", Module: "shell", FreeForm: "uptime"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatal("不应失败")
	}
	jsonRep.Finish()

	out := buf.String()
	if strings.Contains(out, "SECRET-") {
		t.Fatalf("no_log 任务输出泄漏进 JSON 报告:\n%s", out)
	}
	if !strings.Contains(out, "u003credacted") {
		t.Fatalf("no_log 结果应遮蔽为 <redacted>:\n%s", out)
	}
	// 普通任务不受影响（stdout=done）
	if !strings.Contains(out, "done") {
		t.Fatalf("非 no_log 任务输出应正常记录:\n%s", out)
	}
}

// collectReporter 收集结果引用供断言。
type collectReporter struct {
	results []*model.TaskResult
}

func (c *collectReporter) PlayStart(string, []string)            {}
func (c *collectReporter) TaskStart(string, string)              {}
func (c *collectReporter) TaskDone()                             {}
func (c *collectReporter) PlayMsg(string, ...any)                {}
func (c *collectReporter) Recap(string, map[string]*model.Stats) {}
func (c *collectReporter) Finish()                               {}

func (c *collectReporter) HostResult(host string, r *model.TaskResult) {
	c.results = append(c.results, r)
}

// TestNoLogResultFlag no_log 结果携带 NoLog 标记与 output=none（reporter 据此遮蔽）。
func TestNoLogResultFlag(t *testing.T) {
	ex, _ := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0, Stdout: "x"}, nil
	})
	rep := &collectReporter{}
	ex.Rep = rep

	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{{Module: "shell", FreeForm: "id", NoLog: true}},
	}}
	if ex.Run(context.Background(), plays) {
		fmt.Println("run failed")
		t.Fatal("不应失败")
	}
	if len(rep.results) == 0 {
		t.Fatal("未捕获结果")
	}
	for _, r := range rep.results {
		if !r.NoLog || r.Output != "none" {
			t.Fatalf("no_log 结果标记缺失: %+v", r)
		}
	}
}
