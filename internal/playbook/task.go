package playbook

// 任务解析：已知控制属性解析为 Task 字段，剩余唯一键即模块（或 chart 引用）。

import (
	"errors"
	"fmt"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/model"
)

// taskKeys 是任务级已知键（非模块）。注意 chart 是模块键（`chart: 子chart名`），不在此列。
var taskKeys = map[string]bool{
	"name": true, "when": true, "loop": true, "with_items": true,
	"register": true, "notify": true, "tags": true, "environment": true,
	"ignore_errors": true, "retries": true, "delay": true, "timeout": true,
	"become": true, "become_user": true,
	"changed_when": true, "failed_when": true, "args": true,
	"vars": true, "until": true,
	"block": true, "rescue": true, "always": true,
	"output": true, "no_log": true,
	"delegate_to": true, "run_once": true, "loop_control": true, "hook": true,
}

func parseTask(raw any, isHandler bool) (*model.Task, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(i18n.T("task must be a map, got %T", "任务必须是 map，实际为 %T"), raw)
	}
	t := &model.Task{IsHandler: isHandler}
	// name 先解析：后续字段报错时 Label() 能带上任务名
	if v, ok := m["name"]; ok {
		t.Name = fmt.Sprint(v)
	}

	if err := parseTaskFlowKeys(m, t); err != nil {
		return nil, err
	}
	if err := parseTaskContextKeys(m, t); err != nil {
		return nil, err
	}
	if err := parseTaskReportKeys(m, t); err != nil {
		return nil, err
	}
	if err := parseTaskGroups(m, t, isHandler); err != nil {
		return nil, err
	}
	if t.Block != nil {
		t.Module = "block" // 组任务标记（executor 展开执行）
		t.Args = map[string]any{}
		return t, nil
	}
	if t.Rescue != nil || t.Always != nil {
		return nil, fmt.Errorf(i18n.T("task %q: rescue/always must appear together with block", "任务 %q: rescue/always 必须与 block 同时出现"), t.Label())
	}
	return resolveTaskModule(m, t)
}

// parseTaskFlowKeys 解析执行流控制键（when/loop/until/重试/超时）。
func parseTaskFlowKeys(m map[string]any, t *model.Task) error {
	if v, ok := m["when"]; ok {
		l, ok := strOrList(v)
		if !ok {
			return errors.New(i18n.T("when only supports a string or a list", "when 仅支持字符串或列表"))
		}
		t.When = l
	}
	if v, ok := m["loop"]; ok {
		l, err := toAnyList(v, "loop")
		if err != nil {
			return err
		}
		t.Loop = l
	}
	if v, ok := m["with_items"]; ok && t.Loop == nil {
		l, err := toAnyList(v, "with_items")
		if err != nil {
			return err
		}
		t.Loop = l
	}
	if v, ok := m["until"]; ok {
		t.Until = fmt.Sprint(v)
	}
	if v, ok := m["retries"]; ok {
		t.Retries = toInt(v)
	}
	if v, ok := m["delay"]; ok {
		t.DelaySec = toInt(v)
	}
	if v, ok := m["timeout"]; ok {
		t.TimeoutSec = toInt(v)
	}
	if v, ok := m["loop_control"]; ok {
		lc, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf(i18n.T("task %q: loop_control must be a map (loop_var: custom variable name)", "任务 %q: loop_control 必须是 map（loop_var: 自定义变量名）"), t.Label())
		}
		if lv, ok := lc["loop_var"]; ok {
			t.LoopVar = fmt.Sprint(lv)
			if t.LoopVar == "" {
				return fmt.Errorf(i18n.T("task %q: loop_var cannot be empty", "任务 %q: loop_var 不能为空"), t.Label())
			}
		}
	}
	return nil
}

// parseTaskContextKeys 解析执行上下文键（环境变量/提权/委托/错误处理/hook）。
func parseTaskContextKeys(m map[string]any, t *model.Task) error {
	if v, ok := m["environment"]; ok {
		env, err := toStringMap(v)
		if err != nil {
			return fmt.Errorf("environment: %w", err)
		}
		t.Environment = env
	}
	if v, ok := m["ignore_errors"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return fmt.Errorf("任务 %q: ignore_errors: %w", t.Label(), err)
		}
		t.IgnoreErrors = b
	}
	if v, ok := m["become"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return fmt.Errorf("任务 %q: become: %w", t.Label(), err)
		}
		t.Become = &b
	}
	if v, ok := m["become_user"]; ok {
		t.BecomeUser = fmt.Sprint(v)
	}
	if v, ok := m["delegate_to"]; ok {
		t.DelegateTo = fmt.Sprint(v)
	}
	if v, ok := m["run_once"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return fmt.Errorf("任务 %q: run_once: %w", t.Label(), err)
		}
		t.RunOnce = b
	}
	if v, ok := m["hook"]; ok {
		t.Hook = fmt.Sprint(v)
		switch t.Hook {
		case "pre_install", "post_install", "pre_uninstall", "post_uninstall":
		default:
			return fmt.Errorf(i18n.T("task %q: unsupported hook %q (options: pre_install/post_install/pre_uninstall/post_uninstall)", "任务 %q: 不支持的 hook %q（可选: pre_install/post_install/pre_uninstall/post_uninstall）"),
				t.Label(), t.Hook)
		}
	}
	return nil
}

// parseTaskReportKeys 解析结果处理与呈现键（register/notify/tags/判定覆盖/输出控制）。
func parseTaskReportKeys(m map[string]any, t *model.Task) error {
	if v, ok := m["register"]; ok {
		t.Register = fmt.Sprint(v)
	}
	if v, ok := m["notify"]; ok {
		t.Notify, _ = strOrList(v)
	}
	if v, ok := m["tags"]; ok {
		t.Tags, _ = strOrList(v)
	}
	if v, ok := m["changed_when"]; ok {
		t.ChangedWhen = fmt.Sprint(v)
	}
	if v, ok := m["failed_when"]; ok {
		t.FailedWhen = fmt.Sprint(v)
	}
	if v, ok := m["output"]; ok {
		t.Output = fmt.Sprint(v)
		if err := validateOutputSpec(t.Output); err != nil {
			return fmt.Errorf("任务 %q: output: %w", t.Label(), err)
		}
	}
	if v, ok := m["no_log"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return fmt.Errorf("任务 %q: no_log: %w", t.Label(), err)
		}
		t.NoLog = b
	}
	return nil
}

// parseTaskGroups 解析 block/rescue/always 任务组（递归解析，支持嵌套）。
func parseTaskGroups(m map[string]any, t *model.Task, isHandler bool) error {
	for _, key := range []string{"block", "rescue", "always"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		list, err := toList(v, key)
		if err != nil {
			return err
		}
		var tasks []*model.Task
		for _, it := range list {
			bt, err := parseTask(it, isHandler)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			tasks = append(tasks, bt)
		}
		switch key {
		case "block":
			t.Block = tasks
		case "rescue":
			t.Rescue = tasks
		case "always":
			t.Always = tasks
		}
	}
	return nil
}

// resolveTaskModule 解析剩余唯一键为模块（或 chart 引用），合并 vars/args 与简写参数。
func resolveTaskModule(m map[string]any, t *model.Task) (*model.Task, error) {
	if v, ok := m["vars"]; ok {
		cv, err := toAnyMap(v)
		if err != nil {
			return nil, fmt.Errorf("vars: %w", err)
		}
		t.ChartVars = cv
	}
	var explicitArgs map[string]any
	if v, ok := m["args"]; ok {
		am, err := toAnyMap(v)
		if err != nil {
			return nil, fmt.Errorf("args: %w", err)
		}
		explicitArgs = am
	}

	// 剩余唯一键 = 模块（或 chart 引用）
	var modName string
	var modVal any
	for k, v := range m {
		if taskKeys[k] || playKeys[k] {
			continue
		}
		if modName != "" {
			return nil, fmt.Errorf(i18n.T("task %q specifies multiple modules (%s, %s)", "任务 %q 同时指定了多个模块（%s、%s）"), t.Label(), modName, k)
		}
		modName, modVal = k, v
	}
	if modName == "" {
		return nil, fmt.Errorf(i18n.T("task %q does not specify a module", "任务 %q 未指定模块"), t.Label())
	}
	if modName == "chart" {
		// `chart: <子chart名>` 引用，展开执行子 chart 任务序列
		ref, ok := modVal.(string)
		if !ok || ref == "" {
			return nil, fmt.Errorf(i18n.T("task %q: chart reference must be a subchart name string", "任务 %q: chart 引用必须是子 chart 名字符串"), t.Label())
		}
		t.Module = "chart"
		t.ChartRef = ref
		t.Args = map[string]any{}
		return t, nil
	}
	if t.ChartVars != nil {
		return nil, fmt.Errorf(i18n.T("task %q: vars is only for chart reference tasks", "任务 %q: vars 仅用于 chart 引用任务"), t.Label())
	}
	t.Module = modName
	switch x := modVal.(type) {
	case nil:
		t.Args = map[string]any{}
	case string:
		t.FreeForm = x
		t.Args = map[string]any{}
	case map[string]any:
		t.Args = x
	default:
		return nil, fmt.Errorf(i18n.T("module %s parameters must be a string or a map", "模块 %s 的参数必须是字符串或 map"), modName)
	}
	if explicitArgs != nil {
		if t.FreeForm != "" {
			return nil, fmt.Errorf(i18n.T("task %q: shorthand params and args cannot be used together", "任务 %q: 简写参数与 args 不能同时使用"), t.Label())
		}
		for k, v := range explicitArgs {
			t.Args[k] = v
		}
	}
	return t, nil
}

// strOrList 解析字符串或字符串列表字段；其他类型返回 ok=false。
func strOrList(v any) ([]string, bool) {
	switch x := v.(type) {
	case string:
		return []string{x}, true
	case []any:
		var out []string
		for _, s := range x {
			out = append(out, fmt.Sprint(s))
		}
		return out, true
	}
	return nil, false
}

// validateOutputSpec 校验任务级 output 展示控制表达式。
func validateOutputSpec(s string) error {
	switch s {
	case "", "full", "none", "oneline":
		return nil
	}
	for _, p := range []string{"head=", "tail="} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			if rest != "" && strings.Trim(rest, "0123456789") == "" {
				return nil
			}
		}
	}
	return fmt.Errorf(i18n.T("cannot parse %q (use: full/none/oneline/head=N/tail=N)", "无法解析 %q（可用: full/none/oneline/head=N/tail=N）"), s)
}
