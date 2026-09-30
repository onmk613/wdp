package playbook

// 裸 playbook 的静态校验（`wdp lint <playbook.yaml>` 的检测内核）。
// chart 的静态校验在 internal/chart.Lint（chart 依赖本包，Issue 类型
// 各自持有、字段同形，lint 命令统一渲染）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
)

// Issue 是一条校验发现（Level: ERROR/WARN，与 chart.LintIssue 同形）。
type Issue struct {
	Level, Path, Msg string
}

// Lint 对裸 playbook 文件做静态检查：模块名存在性、block 结构递归、chart
// 引用可解析性（playbook 同级 charts/<name>/ 存在）、模板字段 parse-only
// 校验（裸变量引用 {{ var }} 这类运行时才炸的错误提前拦截）。
// 返回发现列表（空 = 通过）。
func Lint(path string) []Issue {
	plays, err := Load(path)
	if err != nil {
		return []Issue{{Level: "ERROR", Path: path, Msg: err.Error()}}
	}
	return lintPlays(plays, path, filepath.Join(filepath.Dir(path), "charts"))
}

// LintContent 对内存中的 playbook 内容做静态检查（web 编辑器校验端点用）。
// 与 Lint 的差异：无文件系统上下文，chart 引用只跳过目录存在性检查
// （编辑器里没有 charts/ 同级目录，检查必然误报；引用名合法性由
// Parse 阶段的 chartref 解析承担）。
func LintContent(data []byte) []Issue {
	plays, err := Parse(data)
	if err != nil {
		return []Issue{{Level: "ERROR", Msg: err.Error()}}
	}
	return lintPlays(plays, "playbook.yaml", "")
}

// lintPlays 是两类入口共用的检查内核。chartsDir 非空时校验 chart 引用
// 目录存在性；空串跳过（无文件上下文）。
func lintPlays(plays []*model.Play, src string, chartsDir string) []Issue {
	eng := render.DefaultEngine()
	var issues []Issue
	var check func(label string, t *model.Task)
	check = func(label string, t *model.Task) {
		if t.ChartRef != "" {
			// 裸 playbook 的引用解析根是同级 charts/ 目录；lint 只验目录
			// 存在性（chart 内容的加载校验由 wdp run 启动期预扫描承担）
			name, _, _ := strings.Cut(t.ChartRef, "@")
			if chartsDir != "" {
				if _, err := os.Stat(filepath.Join(chartsDir, name)); err != nil {
					issues = append(issues, Issue{Level: "ERROR", Path: src,
						Msg: fmt.Sprintf("task %q references chart %q but %s is missing (bare playbooks resolve refs from the charts/ directory next to the playbook)",
							label, t.ChartRef, filepath.Join("charts", name))})
				}
			}
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
			issues = append(issues, Issue{Level: "ERROR", Path: src,
				Msg: fmt.Sprintf("task %q has no module", label)})
			return
		}
		if mod, _, ok := module.Resolve(t.Module, nil); !ok {
			issues = append(issues, Issue{Level: "ERROR", Path: src,
				Msg: fmt.Sprintf("task %q uses unknown module %q", label, t.Module)})
		} else if mod != nil {
			// 空参数调用（如 `set_fact:` 下没写键值对）运行期必失败，提前拦截
			if err := module.LintBareCall(mod, t.Args, t.FreeForm); err != nil {
				issues = append(issues, Issue{Level: "ERROR", Path: src,
					Msg: fmt.Sprintf("task %q: %v", label, err)})
			}
		}
		for _, msg := range eng.ValidateTaskTemplates(t) {
			issues = append(issues, Issue{Level: "ERROR", Path: src,
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
			issues = append(issues, Issue{Level: "ERROR", Path: src,
				Msg: fmt.Sprintf("play %q template: %s", p.Hosts, msg)})
		}
	}
	return issues
}
