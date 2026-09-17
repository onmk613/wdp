package playbook

// include 片段：`include: tasks/fragment.yaml` 在 Load 阶段静态展开
// （等价 ansible import_tasks 语义，非运行期 include_tasks）。
//
// 设计取舍：
//   - 静态展开而非运行期装载——lint/render/--check 天然看到完整任务树，
//     不存在"预演看到的与实际执行的不一致"；坏路径在加载期报错。
//   - 片段共享当前变量域（与子 chart 的 Helm 作用域隔离相反）——
//     它只是"文件级拆分"，不是组件复用；组件复用用 chart 引用。
//   - 片段相对路径以片段自身目录解析（嵌套 include 逐级相对）。

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"wdp/internal/model"
)

// maxIncludeDepth 是 include 嵌套深度上限（防御环引用：a 包含 b、
// b 又包含 a。静态展开的递归无法靠运行期栈保护，必须前置计数拦截）。
const maxIncludeDepth = 32

// expandIncludes 就地展开 plays 中全部 include 片段（任务/handler/block 递归）。
// baseDir 是包含文件的目录，片段相对路径以"包含它的文件"所在目录解析。
func expandIncludes(plays []*model.Play, baseDir string) error {
	var walk func(tasks []*model.Task, dir string, depth int) ([]*model.Task, error)
	walk = func(tasks []*model.Task, dir string, depth int) ([]*model.Task, error) {
		out := make([]*model.Task, 0, len(tasks))
		for _, t := range tasks {
			if t.Include == "" {
				// block/rescue/always 子树同样支持 include
				for _, seg := range []*[]*model.Task{&t.Block, &t.Rescue, &t.Always} {
					if len(*seg) == 0 {
						continue
					}
					expanded, err := walk(*seg, dir, depth)
					if err != nil {
						return nil, err
					}
					*seg = expanded
				}
				out = append(out, t)
				continue
			}
			if depth >= maxIncludeDepth {
				return nil, fmt.Errorf("task %q: include nesting exceeded the depth limit %d (possible cycle)", t.Label(), maxIncludeDepth)
			}
			if t.Loop != nil {
				return nil, fmt.Errorf("task %q: include does not support loop (static expansion; loop belongs on the tasks inside the fragment)", t.Label())
			}
			frag, err := loadFragment(t.Include, dir, t.Label(), t.IsHandler)
			if err != nil {
				return nil, err
			}
			expanded, err := walk(frag, filepath.Dir(resolveIncludePath(t.Include, dir)), depth+1)
			if err != nil {
				return nil, err
			}
			// 控制属性传播（import 语义）：include 上的 when 与片段任务自身
			// 条件 AND 合并、tags 追加、become/delegate_to/run_once 下沉为
			// 片段任务的缺省（片段自身声明优先）——include 上的条件对全部
			// 片段任务生效，而不是在展开时静默丢失。
			for _, ft := range expanded {
				propagateIncludeAttrs(t, ft)
			}
			out = append(out, expanded...)
		}
		return out, nil
	}

	for _, p := range plays {
		tasks, err := walk(p.Tasks, baseDir, 0)
		if err != nil {
			return err
		}
		p.Tasks = tasks
		handlers, err := walk(p.Handlers, baseDir, 0)
		if err != nil {
			return err
		}
		p.Handlers = handlers
	}
	return nil
}

// propagateIncludeAttrs 把 include 任务上的控制属性下沉到片段任务。
func propagateIncludeAttrs(inc, ft *model.Task) {
	if len(inc.When) > 0 {
		ft.When = append(append([]string{}, inc.When...), ft.When...)
	}
	if len(inc.Tags) > 0 {
		ft.Tags = append(append([]string{}, inc.Tags...), ft.Tags...)
	}
	if inc.Become != nil && ft.Become == nil {
		ft.Become = inc.Become
	}
	if inc.BecomeUser != "" && ft.BecomeUser == "" {
		ft.BecomeUser = inc.BecomeUser
	}
	if inc.DelegateTo != "" && ft.DelegateTo == "" {
		ft.DelegateTo = inc.DelegateTo
	}
	if inc.RunOnce {
		ft.RunOnce = true
	}
	if inc.TimeoutSec != 0 && ft.TimeoutSec == 0 {
		ft.TimeoutSec = inc.TimeoutSec
	}
	// 错误处理与展示控制同样下沉：在 `- include: x.yaml` 上写 no_log /
	// ignore_errors 是常见写法，此前这些键被静默丢弃（片段任务"看起来受控"
	// 实则没有）。
	if inc.IgnoreErrors {
		ft.IgnoreErrors = true
	}
	if inc.NoLog {
		ft.NoLog = true
	}
	if inc.Retries != 0 && ft.Retries == 0 {
		ft.Retries = inc.Retries
	}
	if inc.DelaySec != 0 && ft.DelaySec == 0 {
		ft.DelaySec = inc.DelaySec
	}
	if inc.Output != "" && ft.Output == "" {
		ft.Output = inc.Output
	}
	// 环境变量合并：片段任务自身声明优先（更具体的作用域胜出）
	if len(inc.Environment) > 0 {
		merged := map[string]string{}
		maps.Copy(merged, inc.Environment)
		maps.Copy(merged, ft.Environment)
		ft.Environment = merged
	}
}

// resolveIncludePath 解析片段路径：绝对路径原样，相对路径基于包含文件目录。
func resolveIncludePath(include, dir string) string {
	if filepath.IsAbs(include) {
		return filepath.Clean(include)
	}
	return filepath.Clean(filepath.Join(dir, include))
}

// loadFragment 读取并解析片段文件（YAML 任务列表）。
func loadFragment(include, dir, label string, isHandler bool) ([]*model.Task, error) {
	path := resolveIncludePath(include, dir)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("task %q: include %q: %w", label, include, err)
	}
	var raw []any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("task %q: include %q: %w", label, include, err)
	}
	var tasks []*model.Task
	for _, it := range raw {
		t, err := parseTask(it, isHandler)
		if err != nil {
			return nil, fmt.Errorf("include %q: %w", include, err)
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("task %q: include %q contains no tasks", label, include)
	}
	return tasks, nil
}
