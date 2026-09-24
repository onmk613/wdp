package playbook

// 任务解析：已知控制属性解析为 Task 字段，剩余唯一键即模块（或 chart 引用）。

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"wdp/internal/model"
)

// hookRe 是 hook 的语法形态：pre_/post_ + 相位名（首字母字母，其余字母数字
// 下划线连字符；deploy 相位沿用 install 词干）。此处只校验语法，"相位是否
// 存在"由 chart lint 对照根目录 <phase>.yaml 判定（裸 playbook 无相位文件
// 概念，任意合法命名的 hook 仅在同相位执行时才会被选中）。
var hookRe = regexp.MustCompile(`^(pre|post)_[A-Za-z][A-Za-z0-9_-]*$`)

// taskKeys 是任务级已知键（非模块）——由 TaskFieldSections 文档表派生：
// 字段表是控制键的唯一清单（新增键在 taskdoc.go 写 FieldDoc 并在下方
// parseTask* 补解析，TestTaskFieldsPopulateStruct 反射校验"文档键必被
// 解析进声称的 model.Task 字段"）。chart/include 是模块键位置的写法，
// values/values_from/hosts/phase 是 chart 引用任务的专属键（由
// parseChartRefTask 消费）——都不进白名单：普通模块任务携带这些键会
// 被当成模块名报错，而不是静默吞掉。
var taskKeys = docTaskKeys()

// moduleKeyWrites 是写在模块位置的引用写法（不占用控制键白名单）。
var moduleKeyWrites = map[string]bool{"chart": true, "include": true}

// chartRefOnlyKeys 是 chart 引用任务的专属配置键（不进通用控制键白名单）。
var chartRefOnlyKeys = map[string]bool{"values": true, "values_from": true, "hosts": true, "phase": true}

func docTaskKeys() map[string]bool {
	out := map[string]bool{}
	for _, sec := range TaskFieldSections() {
		for _, f := range sec.Fields {
			if !moduleKeyWrites[f.Name] && !chartRefOnlyKeys[f.Name] {
				out[f.Name] = true
			}
		}
	}
	return out
}

func parseTask(raw any, isHandler bool) (*model.Task, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("task must be a map, got %T", raw)
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
		// 组任务只接受控制属性：模块键（shell/copy/…）、chart/include 引用与
		// 其它未知键此前被静默丢弃——写错层级时任务"看起来生效"实则没跑。
		for k := range m {
			if k == "name" || k == "block" || k == "rescue" || k == "always" || taskKeys[k] {
				continue
			}
			return nil, fmt.Errorf("task %q: block task does not accept key %q (module keys, chart/include references and their args belong inside the block's subtasks)", t.Label(), k)
		}
		t.Module = "block" // 组任务标记（executor 展开执行）
		t.Args = map[string]any{}
		return t, nil
	}
	if t.Rescue != nil || t.Always != nil {
		return nil, fmt.Errorf("task %q: rescue/always must appear together with block", t.Label())
	}
	return resolveTaskModule(m, t)
}

// parseTaskFlowKeys 解析执行流控制键（when/loop/until/重试/超时）。
func parseTaskFlowKeys(m map[string]any, t *model.Task) error {
	if v, ok := m["when"]; ok {
		l, ok := strOrList(v)
		if !ok {
			return errors.New("when only supports a string or a list")
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
	if v, ok := m["until"]; ok {
		t.Until = fmt.Sprint(v)
	}
	if v, ok := m["retries"]; ok {
		n, err := toInt(v)
		if err != nil {
			return fmt.Errorf("task %q: retries: %w", t.Label(), err)
		}
		t.Retries = n
	}
	if v, ok := m["delay"]; ok {
		n, err := toInt(v)
		if err != nil {
			return fmt.Errorf("task %q: delay: %w", t.Label(), err)
		}
		t.DelaySec = n
	}
	if v, ok := m["timeout"]; ok {
		n, err := toInt(v)
		if err != nil {
			return fmt.Errorf("task %q: timeout: %w", t.Label(), err)
		}
		t.TimeoutSec = n
	}
	if v, ok := m["loop_control"]; ok {
		lc, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("task %q: loop_control must be a map (loop_var: custom variable name)", t.Label())
		}
		if lv, ok := lc["loop_var"]; ok {
			t.LoopVar = fmt.Sprint(lv)
			if t.LoopVar == "" {
				return fmt.Errorf("task %q: loop_var cannot be empty", t.Label())
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
			return fmt.Errorf("task %q: ignore_errors: %w", t.Label(), err)
		}
		t.IgnoreErrors = b
	}
	if v, ok := m["become"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return fmt.Errorf("task %q: become: %w", t.Label(), err)
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
			return fmt.Errorf("task %q: run_once: %w", t.Label(), err)
		}
		t.RunOnce = b
	}
	if v, ok := m["hook"]; ok {
		t.Hook = fmt.Sprint(v)
		if !hookRe.MatchString(t.Hook) {
			return fmt.Errorf("task %q: unsupported hook %q (expected pre_<phase>/post_<phase>, e.g. pre_install/pre_update)",
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
		list, ok2 := strOrList(v)
		if !ok2 {
			return fmt.Errorf("notify: expected a string or a list of strings, got %T", v) // 吞错会让 handler 永不触发
		}
		t.Notify = list
	}
	if v, ok := m["tags"]; ok {
		list, ok2 := strOrList(v)
		if !ok2 {
			return fmt.Errorf("tags: expected a string or a list of strings, got %T", v)
		}
		t.Tags = list
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
			return fmt.Errorf("task %q: output: %w", t.Label(), err)
		}
	}
	if v, ok := m["no_log"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return fmt.Errorf("task %q: no_log: %w", t.Label(), err)
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
	if v, ok := m["tasks_from"]; ok {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("task %q: tasks_from must be a non-empty phase name string", t.Label())
		}
		t.TasksFrom = s
	}
	var explicitArgs map[string]any
	if v, ok := m["args"]; ok {
		am, err := toAnyMap(v)
		if err != nil {
			return nil, fmt.Errorf("args: %w", err)
		}
		explicitArgs = am
	}

	// 剩余唯一键 = 模块（或 chart 引用 / include 片段引用）。
	// chart 引用提前分支：其专属键（values/values_from/hosts/phase）若落进
	// 通用扫描会被当成第二个模块键误报，必须在扫描前消费。
	if cv, ok := m["chart"]; ok {
		return parseChartRefTask(m, cv, t)
	}
	var modName string
	var modVal any
	var include string
	for k, v := range m {
		if taskKeys[k] || playKeys[k] {
			continue
		}
		if k == "include" {
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("task %q: include must be a non-empty path string", t.Label())
			}
			include = s
			continue
		}
		if modName != "" {
			return nil, fmt.Errorf("task %q specifies multiple modules (%s, %s)", t.Label(), modName, k)
		}
		modName, modVal = k, v
	}
	if include != "" && modName != "" {
		return nil, fmt.Errorf("task %q: include cannot be combined with module %s", t.Label(), modName)
	}
	if include != "" {
		// include 片段（`include: tasks/x.yaml`）：Load 阶段静态展开，
		// 展开后此占位任务被片段任务序列替换（executor 不会见到它）
		t.Module = "include"
		t.Include = include
		t.Args = map[string]any{}
		return t, nil
	}
	if modName == "" {
		return nil, fmt.Errorf("task %q does not specify a module", t.Label())
	}
	if t.ChartVars != nil {
		return nil, fmt.Errorf("task %q: vars is only for chart reference tasks", t.Label())
	}
	if t.TasksFrom != "" {
		return nil, fmt.Errorf("task %q: tasks_from is only for chart reference tasks", t.Label())
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
		return nil, fmt.Errorf("module %s parameters must be a string or a map", modName)
	}
	if explicitArgs != nil {
		if t.FreeForm != "" {
			return nil, fmt.Errorf("task %q: shorthand params and args cannot be used together", t.Label())
		}
		maps.Copy(t.Args, explicitArgs)
	}
	return t, nil
}

// chartRefKeys 是 chart 引用任务的专属键（不进通用控制键白名单——
// 普通模块任务携带这些键应报"未知模块"而非静默吞掉）。
// 派生自 chartRefOnlyKeys + 引用键本身：两份字面量并存时新增键漏改
// 一处即产生文档/解析口径漂移。
var chartRefKeys = func() map[string]bool {
	m := maps.Clone(chartRefOnlyKeys)
	m["chart"] = true
	return m
}()

// parseChartRefTask 解析 chart 引用任务（唯一形态——map 集中配置）：
//
//   - chart: {name: jdk, values: {...}, values_from: [...], hosts: ..., phase: ...}
//
// 曾并存 `chart: jdk` 简写（引用名直写、配置键平级），已移除：两种写法
// 并存让文档/补全/校验都要双份口径，统一为 map 形态。
//
// chart 模式下引用名解析为父 chart 的子 chart；裸 playbook 模式解析为
// playbook 同级 charts/<name>/ 目录（wdp run 启动期预扫描加载）。
// hosts 是当前 play 批次内的过滤选择器（不跨 play 重选主机）；values 与
// vars、phase 与 tasks_from 是同义键，同任务内不可混用（防两处配置漂移）。
func parseChartRefTask(m map[string]any, modVal any, t *model.Task) (*model.Task, error) {
	t.Module = "chart"
	setVars := func(v any, key string) error {
		cv, err := toAnyMap(v)
		if err != nil {
			return fmt.Errorf("task %q: %s: %w", t.Label(), key, err)
		}
		if t.ChartVars != nil {
			return fmt.Errorf("task %q: vars and values cannot be used together (they are the same setting)", t.Label())
		}
		t.ChartVars = cv
		return nil
	}
	setPhase := func(v any, key string) error {
		s, ok := v.(string)
		if !ok || s == "" {
			return fmt.Errorf("task %q: %s must be a non-empty phase name string", t.Label(), key)
		}
		if t.TasksFrom != "" {
			return fmt.Errorf("task %q: tasks_from and phase cannot be used together (they are the same setting)", t.Label())
		}
		t.TasksFrom = s
		return nil
	}
	setRef := func(ref any) error {
		s, ok := ref.(string)
		if !ok || s == "" {
			return fmt.Errorf("task %q: chart reference name must be a non-empty string", t.Label())
		}
		t.ChartRef = s
		return nil
	}
	apply := func(k string, v any) error {
		switch k {
		case "name":
			return setRef(v)
		case "values":
			return setVars(v, "values")
		case "values_from":
			l, ok := strOrList(v)
			if !ok {
				return fmt.Errorf("task %q: values_from only supports a string or a list of strings", t.Label())
			}
			t.ChartValuesFrom = l
			return nil
		case "hosts":
			t.ChartHosts = fmt.Sprint(v)
			return nil
		case "phase":
			return setPhase(v, "phase")
		case "tasks_from":
			return setPhase(v, "tasks_from")
		case "vars":
			return setVars(v, "vars")
		}
		return nil
	}

	if cfg, ok := modVal.(map[string]any); ok {
		// map 形态：配置键只认 map 内的；与平级写法混用（map 外再出现
		// values/hosts 等）属两处配置漂移，直接拒绝
		for k := range cfg {
			if k == "chart" {
				return nil, fmt.Errorf("task %q: chart map cannot nest a chart key", t.Label())
			}
			if k != "name" && (taskKeys[k] || k == "include") {
				return nil, fmt.Errorf("task %q: key %q belongs at the task level, not inside the chart map", t.Label(), k)
			}
			if !chartRefKeys[k] && k != "name" && k != "tasks_from" && k != "vars" {
				return nil, fmt.Errorf("task %q: unknown chart reference key %q (supported: name/values/values_from/hosts/phase)", t.Label(), k)
			}
		}
		for _, k := range []string{"name", "values", "values_from", "hosts", "phase", "tasks_from", "vars"} {
			if v, ok := cfg[k]; ok {
				if err := apply(k, v); err != nil {
					return nil, err
				}
			}
		}
		if t.ChartRef == "" {
			return nil, fmt.Errorf("task %q: chart map form requires a name (e.g. chart: {name: jdk})", t.Label())
		}
	} else {
		// 简写形态（chart: jdk）已移除：报错给迁移提示而不是静默失败
		return nil, fmt.Errorf("task %q: chart shorthand (chart: <name>) was removed; use the map form: chart: {name: %v, ...}", t.Label(), modVal)
	}

	// 其余未知键一律拒绝（普通模块键混进 chart 引用任务会静默失效）；
	// chart 专属键出现在平级位置同样拒绝——配置集中在 map 里
	for k := range m {
		if taskKeys[k] || k == "chart" || k == "name" || k == "vars" || k == "tasks_from" {
			continue
		}
		if chartRefOnlyKeys[k] {
			return nil, fmt.Errorf("task %q: key %q belongs inside the chart map (chart: {name: ..., %s: ...})", t.Label(), k, k)
		}
		return nil, fmt.Errorf("task %q: chart reference does not accept key %q (module tasks and chart references cannot be combined)", t.Label(), k)
	}
	t.Args = map[string]any{}
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
	return fmt.Errorf("cannot parse %q (use: full/none/oneline/head=N/tail=N)", s)
}
