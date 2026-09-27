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
	Line  int // 相关行号（1 起，0 = 未知/不适用）：任务级发现由 Task.Line 带出
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
		return append(issues, LintIssue{ERROR, "_helpers.tpl", err.Error(), 0}) // 引擎不可用时后续模板校验无意义
	}

	issues = append(issues, lintTaskTree(c, eng)...)
	issues = append(issues, lintPhaseDecls(c)...)
	issues = append(issues, lintTemplates(c, eng, values)...)
	issues = append(issues, lintEnvs(c)...)
	issues = append(issues, lintSchemas(c, values)...)
	issues = append(issues, lintInventoryOverride(c, values)...)
	return issues
}

// lintTaskTree 走查全部相位（deploy + uninstall/status/自定义相位文件，含子
// chart 递归）的任务树：hook 名、chart 引用、模块名与模板字段。
// 此前只查 deploy.yaml，相位文件里的未知模块与坏模板要到运行时才暴露。
func lintTaskTree(c *Chart, eng *render.Engine) []LintIssue {
	var issues []LintIssue
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
					fmt.Sprintf("task %q hook %q matches no phase (expected pre_/post_ + one of: %s)", label, t.Hook, strings.Join(c.PhaseNames(), ", ")), t.Line})
			}
			if t.ChartRef != "" {
				sub, serr := c.ResolveSub(t.ChartRef)
				if serr != nil {
					issues = append(issues, LintIssue{ERROR, path,
						fmt.Sprintf("task %q failed to reference subchart: %v", label, serr), t.Line})
				} else if _, perr := sub.EntryPlay(t.TasksFrom); perr != nil {
					issues = append(issues, LintIssue{ERROR, path,
						fmt.Sprintf("task %q tasks_from: %v", label, perr), t.Line})
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
					fmt.Sprintf("task %q uses unknown module %q", label, t.Module), t.Line})
			}
			// 模板字段 parse-only 校验（裸变量引用 {{ var }} 这类运行时才炸的错误提前拦截）
			for _, msg := range eng.ValidateTaskTemplates(t) {
				issues = append(issues, LintIssue{ERROR, path,
					fmt.Sprintf("task %q template: %s", label, msg), t.Line})
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
		// 子 chart 按名字典序遍历：map 迭代随机会让 lint 输出顺序不稳定
		//（包内其它遍历统一走 sortedSubNames）
		for _, name := range sortedSubNames(ch.Subs) {
			walk(name+".", ch.Subs[name])
		}
	}
	walk("", c)
	return issues
}

// lintPhaseDecls 校验 chart.yaml phases 声明与相位文件匹配：声明了没有
// 对应文件的相位大概率是拼写错误（该声明永远不会生效）——与
// inventory_override 同样的静默失效防御。
func lintPhaseDecls(c *Chart) []LintIssue {
	var issues []LintIssue
	for _, p := range slices.Sorted(maps.Keys(c.Meta.Phases)) {
		if _, ok := c.Phases[p]; !ok {
			if _, builtin := builtinPhaseSpecs[p]; !builtin {
				issues = append(issues, LintIssue{WARN, "chart.yaml",
					fmt.Sprintf("phases.%s has no matching %s.yaml file (typo? the declaration never applies)", p, p), 0})
			}
		}
	}
	return issues
}

// lintTemplates 校验模板文件可渲染（样例域：合并 values + 占位主机名）。
// 模板列举失败（templates/ 存在但目录不可读等）直接 ERROR：吞掉会误报
// 全绿，渲染时才发现模板缺失。
func lintTemplates(c *Chart, eng *render.Engine, values map[string]any) []LintIssue {
	var issues []LintIssue
	tplFiles, terr := c.walkTemplates()
	if terr != nil {
		issues = append(issues, LintIssue{ERROR, "templates",
			fmt.Sprintf("failed to list templates: %v", terr), 0})
	}
	sample := map[string]any{}
	maps.Copy(sample, values)
	sample["inventory_hostname"] = "lint-host"
	for _, rel := range tplFiles {
		data, err := os.ReadFile(filepath.Join(c.Dir, rel))
		if err != nil {
			issues = append(issues, LintIssue{ERROR, rel, err.Error(), 0})
			continue
		}
		if _, err := eng.Render(string(data), sample); err != nil {
			issues = append(issues, LintIssue{ERROR, rel, err.Error(), 0})
		}
	}
	return issues
}

// lintEnvs 校验 envs 文件可解析；且叠加默认 values 后要过根 chart schema
// （defaults+env 即该环境实际生效的静态域，不实际运行即可暴露配置错误）。
func lintEnvs(c *Chart) []LintIssue {
	var issues []LintIssue
	for _, env := range c.EnvFiles() {
		data, err := os.ReadFile(filepath.Join(c.Dir, "envs", env))
		if err == nil {
			var ov map[string]any
			ov, err = LoadValuesYAML(data)
			if err == nil {
				// Merge 首行即深拷贝 base，此处无需再拷贝 c.Values
				if serr := c.ValidateValuesSchema(Merge(c.Values, ov)); serr != nil {
					issues = append(issues, LintIssue{ERROR, "envs/" + env, serr.Error(), 0})
				}
			}
		}
		if err != nil {
			issues = append(issues, LintIssue{ERROR, "envs/" + env, err.Error(), 0})
		}
	}
	return issues
}

// lintSchemas 校验 values.schema.json：合并 values（defaults + -f + --set）
// 过根 chart schema；子 chart 逐层静态走查（SubScope 算域——父 values 里
// <子名> 子树的类型错误在此暴露，运行期由 executor 用含引用 vars 的精确
// 作用域再校验）。
func lintSchemas(c *Chart, values map[string]any) []LintIssue {
	var issues []LintIssue
	if err := c.ValidateValuesSchema(values); err != nil {
		issues = append(issues, LintIssue{ERROR, SchemaFile, err.Error(), 0})
	}
	if err := c.ValidateSubchartsSchema(values); err != nil {
		issues = append(issues, LintIssue{ERROR, SchemaFile, err.Error(), 0})
	}
	return issues
}

// lintInventoryOverride 校验 inventory_override 白名单键是 values 顶层键
// （合并 -f/--set 后判定）：列了不存在的键大概率是拼写错误——该键会被
// 静默忽略，主机的同名变量依旧被 values 遮蔽（如果 values 里根本没有
// 这个键则白名单无意义）。
func lintInventoryOverride(c *Chart, values map[string]any) []LintIssue {
	var issues []LintIssue
	for _, k := range c.Meta.InventoryOverride {
		if _, ok := values[k]; !ok {
			issues = append(issues, LintIssue{WARN, "chart.yaml",
				fmt.Sprintf("inventory_override key %q is not a top-level values key (typo? overrides for it never apply)", k), 0})
		}
	}
	return issues
}
