package chart

// chart 静态校验（`wdp lint` 的检测内核）：结构、模块名、chart 引用、
// 模板可渲染、envs 可解析。裸 playbook 的校验在 internal/playbook.Lint。

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
)

const (
	ERROR = "ERROR"
	WARN  = "WARN"
)

// LintIssue 是一条校验发现。
type LintIssue struct {
	Level string // ERROR / WARN
	Path  string // 相关文件
	Msg   string
}

// String 渲染 LintIssue。
func (i LintIssue) String() string {
	return fmt.Sprintf("[%s] %s: %s", i.Level, i.Path, i.Msg)
}

// Lint 静态校验 chart：结构、模块名、chart 引用、模板可渲染（用给定 values）、envs 可解析。
// 返回全部发现（可能为空）。
func Lint(c *Chart, values map[string]any) []LintIssue {
	var issues []LintIssue

	// helpers 可解析（含子 chart 合并）
	eng, err := render.NewEngine(c.CollectHelpers())
	if err != nil {
		issues = append(issues, LintIssue{ERROR, "_helpers.tpl", err.Error()})
		return issues // 引擎不可用时后续模板校验无意义
	}

	// deploy 任务树校验（含子 chart 引用、模块名与 block 组递归）
	var walk func(prefix string, ch *Chart)
	walk = func(prefix string, ch *Chart) {
		var checkTask func(label string, t *model.Task)
		checkTask = func(label string, t *model.Task) {
			if t.ChartRef != "" {
				if _, err := c.ResolveSub(t.ChartRef); err != nil {
					issues = append(issues, LintIssue{ERROR, "deploy.yaml",
						fmt.Sprintf("task %q failed to reference subchart: %v", label, err)})
				}
				return
			}
			if t.Module == "block" {
				for _, seg := range [][]*model.Task{t.Block, t.Rescue, t.Always} {
					for _, ct := range seg {
						checkTask(label+"."+ct.Label(), ct)
					}
				}
				return
			}
			// 模块解析唯一规则（module.Resolve）：内置优先，chart 本地脚本模块视为合法
			if _, _, ok := module.Resolve(t.Module, []string{ch.Dir, c.Dir}); !ok {
				issues = append(issues, LintIssue{ERROR, "deploy.yaml",
					fmt.Sprintf("task %q uses unknown module %q", label, t.Module)})
			}
			// 模板字段 parse-only 校验（裸变量引用 {{ var }} 这类运行时才炸的错误提前拦截）
			for _, msg := range eng.ValidateTaskTemplates(t) {
				issues = append(issues, LintIssue{ERROR, "deploy.yaml",
					fmt.Sprintf("task %q template: %s", label, msg)})
			}
		}
		for _, play := range ch.Deploy {
			tasks := append(append([]*model.Task{}, play.Tasks...), play.Handlers...)
			for _, t := range tasks {
				checkTask(prefix+t.Label(), t)
			}
		}
		for name, sub := range ch.Subs {
			walk(name+".", sub)
		}
	}
	walk("", c)

	// 模板文件可渲染（样例域：合并 values + 占位主机名）
	sample := map[string]any{}
	maps.Copy(sample, values)
	sample["inventory_hostname"] = "lint-host"
	for _, rel := range c.TemplateFiles() {
		data, err := os.ReadFile(filepath.Join(c.Dir, rel))
		if err != nil {
			issues = append(issues, LintIssue{ERROR, rel, err.Error()})
			continue
		}
		if _, err := eng.Render(string(data), sample); err != nil {
			issues = append(issues, LintIssue{ERROR, rel, err.Error()})
		}
	}

	// envs 文件可解析
	for _, env := range c.EnvFiles() {
		data, err := os.ReadFile(filepath.Join(c.Dir, "envs", env))
		if err == nil {
			_, err = LoadValuesYAML(data)
		}
		if err != nil {
			issues = append(issues, LintIssue{ERROR, "envs/" + env, err.Error()})
		}
	}

	// inventory_override 白名单键必须是 values 顶层键（合并 -f/--set 后判定）：
	// 列了不存在的键大概率是拼写错误——该键会被静默忽略，主机的同名变量
	// 依旧被 values 遮蔽（如果 values 里根本没有这个键则白名单无意义）。
	for _, k := range c.Meta.InventoryOverride {
		if _, ok := values[k]; !ok {
			issues = append(issues, LintIssue{WARN, "chart.yaml",
				fmt.Sprintf("inventory_override key %q is not a top-level values key (typo? overrides for it never apply)", k)})
		}
	}
	return issues
}
