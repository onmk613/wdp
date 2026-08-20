package report

import (
	"encoding/json"
	"io"
	"sync"

	"wdp/internal/model"
)

// JSONTask 是单个任务的全部主机结果。
type JSONTask struct {
	Name    string              `json:"name"`
	Module  string              `json:"module"`
	Results []*model.TaskResult `json:"results"`
	Summary *model.Stats        `json:"summary"`
}

// JSONPlay 是单个 play 的完整执行记录。
type JSONPlay struct {
	Name  string                  `json:"name"`
	Hosts []string                `json:"hosts"`
	Tasks []*JSONTask             `json:"tasks"`
	Recap map[string]*model.Stats `json:"recap"`
}

// JSONReporter 输出机器可读的完整执行记录（--output json，适配 CI/CD）。
// stdout 只输出最终 JSON（进度信息不打印），错误仍走 stderr。
type JSONReporter struct {
	mu      sync.Mutex
	out     io.Writer
	plays   []*JSONPlay
	curPlay *JSONPlay
	curTask *JSONTask
}

// NewJSONReporter 创建 JSON reporter。
func NewJSONReporter(out io.Writer) *JSONReporter {
	return &JSONReporter{out: out}
}

// PlayStart 开始一个 play。
func (r *JSONReporter) PlayStart(name string, hosts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.curPlay = &JSONPlay{Name: name, Hosts: hosts, Recap: map[string]*model.Stats{}}
	r.plays = append(r.plays, r.curPlay)
}

// TaskStart 开始一个任务。
func (r *JSONReporter) TaskStart(task, module string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.curPlay == nil {
		return
	}
	r.curTask = &JSONTask{Name: task, Module: module, Summary: &model.Stats{}}
	r.curPlay.Tasks = append(r.curPlay.Tasks, r.curTask)
}

// HostResult 记录单主机结果。
// no_log 任务遮蔽 stdout/stderr/msg/diff（含 loop 逐项）：
// JSON 报告常落 CI 工件，敏感输出不得随报告持久化（register 变量不受影响）。
func (r *JSONReporter) HostResult(host string, res *model.TaskResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.curTask == nil {
		return
	}
	if res.NoLog {
		res = redact(res)
	}
	r.curTask.Results = append(r.curTask.Results, res)
	switch {
	case res.Unreachable:
		r.curTask.Summary.Unreachable++
	case res.Failed:
		r.curTask.Summary.Failed++
	case res.Skipped:
		r.curTask.Summary.Skipped++
	case res.Changed:
		r.curTask.Summary.Changed++
	default:
		r.curTask.Summary.Ok++
	}
}

// redacted 是 no_log 字段的统一遮蔽占位。
const redacted = "<redacted>"

// redact 返回遮蔽副本（不改原结果：register 数据在内存中保持完整）。
// 父任务 no_log 时逐项结果无条件遮蔽——项内容同样来自敏感输出。
func redact(res *model.TaskResult) *model.TaskResult {
	c := *res
	c.NoLog = true
	c.Stdout, c.Stderr, c.Msg, c.Diff = redacted, redacted, redacted, ""
	if len(res.Items) > 0 {
		c.Items = make([]*model.TaskResult, len(res.Items))
		for i, it := range res.Items {
			c.Items[i] = redact(it)
		}
	}
	return &c
}

// TaskDone 结束当前任务（JSON 模式无需处理）。
func (r *JSONReporter) TaskDone() {}

// PlayMsg 忽略进度消息（JSON 仅含结构化结果）。
func (r *JSONReporter) PlayMsg(format string, a ...any) {}

// Recap 记录 play 汇总。
func (r *JSONReporter) Recap(playName string, stats map[string]*model.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.curPlay != nil {
		r.curPlay.Recap = stats
	}
}

// Finish 输出最终 JSON 文档（整个 run 结束时调用一次）。
func (r *JSONReporter) Finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plays == nil {
		r.plays = []*JSONPlay{}
	}
	enc := json.NewEncoder(r.out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"plays": r.plays})
}
