package chart

// chart 静态校验（`wdp lint` 的检测内核）：结构、模块名、chart 引用、
// 模板可渲染、envs 可解析。裸 playbook 的校验在 internal/playbook.Lint。

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

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

	// 全相位任务树校验（deploy + uninstall/status/自定义相位文件；含子
	// chart 引用、模块名与 block 组递归）——此前只查 deploy.yaml，相位
	// 文件里的未知模块与坏模板要到运行时才暴露
	validHooks := map[string]bool{}
	for _, p := range c.PhaseNames() {
		validHooks["pre_"+HookNameFor(p)] = true
		validHooks["post_"+HookNameFor(p)] = true
	}
	var walk func(prefix string, ch *Chart)
	walk = func(prefix string, ch *Chart) {
		var checkTask func(path, label string, t *model.Task)
		checkTask = func(path, label string, t *model.Task) {
			if t.Hook != "" && !validHooks[t.Hook] {
				issues = append(issues, LintIssue{WARN, path,
					fmt.Sprintf("task %q hook %q matches no phase (expected pre_/post_ + one of: %s)", label, t.Hook, strings.Join(c.PhaseNames(), ", "))})
			}
			if t.ChartRef != "" {
				sub, serr := c.ResolveSub(t.ChartRef)
				if serr != nil {
					issues = append(issues, LintIssue{ERROR, path,
						fmt.Sprintf("task %q failed to reference subchart: %v", label, serr)})
				} else if _, perr := sub.EntryPlay(t.TasksFrom); perr != nil {
					issues = append(issues, LintIssue{ERROR, path,
						fmt.Sprintf("task %q tasks_from: %v", label, perr)})
				}
				return
			}
			if t.Module == "block" {
				for _, seg := range [][]*model.Task{t.Block, t.Rescue, t.Always} {
					for _, ct := range seg {
						checkTask(path, label+"."+ct.Label(), ct)
					}
				}
				return
			}
			// 模块解析唯一规则（module.Resolve）：内置优先，chart 本地脚本模块视为合法
			if _, _, ok := module.Resolve(t.Module, []string{ch.Dir, c.Dir}); !ok {
				issues = append(issues, LintIssue{ERROR, path,
					fmt.Sprintf("task %q uses unknown module %q", label, t.Module)})
			}
			// 模板字段 parse-only 校验（裸变量引用 {{ var }} 这类运行时才炸的错误提前拦截）
			for _, msg := range eng.ValidateTaskTemplates(t) {
				issues = append(issues, LintIssue{ERROR, path,
					fmt.Sprintf("task %q template: %s", label, msg)})
			}
		}
		for _, pf := range ch.phaseFiles() {
			for _, play := range pf.plays {
				tasks := append(append([]*model.Task{}, play.Tasks...), play.Handlers...)
				for _, t := range tasks {
					checkTask(pf.name, t.Label(), t)
				}
			}
		}
		for name, sub := range ch.Subs {
			walk(name+".", sub)
		}
	}
	walk("", c)

	// chart.yaml phases 声明与相位文件匹配：声明了没有对应文件的相位大概率
	// 是拼写错误（该声明永远不会生效）——与 inventory_override 同样的静默失效防御
	for _, p := range slices.Sorted(maps.Keys(c.Meta.Phases)) {
		if _, ok := c.Phases[p]; !ok {
			if _, builtin := builtinPhaseSpecs[p]; !builtin {
				issues = append(issues, LintIssue{WARN, "chart.yaml",
					fmt.Sprintf("phases.%s has no matching %s.yaml file (typo? the declaration never applies)", p, p)})
			}
		}
	}

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

	// envs 文件可解析；且叠加默认 values 后要过根 chart schema
	// （defaults+env 即该环境实际生效的静态域，不实际运行即可暴露配置错误）
	for _, env := range c.EnvFiles() {
		data, err := os.ReadFile(filepath.Join(c.Dir, "envs", env))
		if err == nil {
			var ov map[string]any
			ov, err = LoadValuesYAML(data)
			if err == nil {
				if serr := c.ValidateValuesSchema(Merge(deepCopyValues(c.Values), ov)); serr != nil {
					issues = append(issues, LintIssue{ERROR, "envs/" + env, serr.Error()})
				}
			}
		}
		if err != nil {
			issues = append(issues, LintIssue{ERROR, "envs/" + env, err.Error()})
		}
	}

	// values.schema.json：合并 values（defaults + -f + --set）过根 chart schema；
	// 子 chart 逐层静态走查（SubScope 算域——父 values 里 <子名> 子树的类型
	// 错误在此暴露，运行期由 executor 用含引用 vars 的精确作用域再校验）
	if err := c.ValidateValuesSchema(values); err != nil {
		issues = append(issues, LintIssue{ERROR, SchemaFile, err.Error()})
	}
	if err := c.ValidateSubchartsSchema(values); err != nil {
		issues = append(issues, LintIssue{ERROR, SchemaFile, err.Error()})
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
