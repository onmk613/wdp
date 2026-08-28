package playbook

// 裸 playbook 的静态校验（`wdp lint <playbook.yaml>` 的检测内核）。
// chart 的静态校验在 internal/chart.Lint（chart 依赖本包，Issue 类型
// 各自持有、字段同形，lint 命令统一渲染）。

import (
	"fmt"

	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
)

// Issue 是一条校验发现（Level: ERROR/WARN，与 chart.LintIssue 同形）。
type Issue struct {
	Level, Path, Msg string
}

// Lint 对裸 playbook 做静态检查：模块名存在性、block 结构递归、
// chart 引用拒绝（那是 chart 语境的能力）、模板字段 parse-only 校验
// （裸变量引用 {{ var }} 这类运行时才炸的错误提前拦截）。
// 返回发现列表（空 = 通过）。
func Lint(path string) []Issue {
	plays, err := Load(path)
	if err != nil {
		return []Issue{{Level: "ERROR", Path: path, Msg: err.Error()}}
	}
	eng := render.DefaultEngine()
	var issues []Issue
	var check func(label string, t *model.Task)
	check = func(label string, t *model.Task) {
		if t.ChartRef != "" {
			issues = append(issues, Issue{Level: "ERROR", Path: path,
				Msg: fmt.Sprintf("task %q uses chart ref %q — subchart references only work inside a chart deploy.yaml", label, t.ChartRef)})
			return
		}
		if t.Module == "block" {
			for _, seg := range [][]*model.Task{t.Block, t.Rescue, t.Always} {
				for _, ct := range seg {
					check(label+"."+ct.Label(), ct)
				}
			}
			return
		}
		if t.Module == "" {
			issues = append(issues, Issue{Level: "ERROR", Path: path,
				Msg: fmt.Sprintf("task %q has no module", label)})
			return
		}
		if _, _, ok := module.Resolve(t.Module, nil); !ok {
			issues = append(issues, Issue{Level: "ERROR", Path: path,
				Msg: fmt.Sprintf("task %q uses unknown module %q", label, t.Module)})
		}
		for _, msg := range eng.ValidateTaskTemplates(t) {
			issues = append(issues, Issue{Level: "ERROR", Path: path,
				Msg: fmt.Sprintf("task %q template: %s", label, msg)})
		}
	}
	for _, p := range plays {
		for _, t := range p.Tasks {
			check(t.Label(), t)
		}
		for _, t := range p.Handlers {
			check("handler."+t.Label(), t)
		}
		for _, msg := range eng.ValidateEnv(p.Environment) {
			issues = append(issues, Issue{Level: "ERROR", Path: path,
				Msg: fmt.Sprintf("play %q template: %s", p.Hosts, msg)})
		}
	}
	return issues
}
