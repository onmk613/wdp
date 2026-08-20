// Package playbook 解析 Ansible 风格的 playbook YAML。
// 每个任务是单键 map：已知控制属性之外的唯一一个键即模块名。
package playbook

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"wdp/internal/model"
)

// Load 从文件解析 playbook。
func Load(path string) ([]*model.Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 playbook 失败: %w", err)
	}
	plays, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for _, p := range plays {
		for _, t := range append(append([]*model.Task{}, p.Tasks...), p.Handlers...) {
			if t.Module == "" {
				return nil, fmt.Errorf("play %s 存在未指定模块的任务", p.Name)
			}
		}
	}
	return plays, nil
}

// Parse 解析 playbook 内容。
func Parse(data []byte) ([]*model.Play, error) {
	var raw []map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 playbook 失败: %w", err)
	}
	plays := make([]*model.Play, 0, len(raw))
	for i, rp := range raw {
		p, err := parsePlay(rp)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个 play: %w", i+1, err)
		}
		plays = append(plays, p)
	}
	return plays, nil
}

// playKeys 是 play 级已知键。
var playKeys = map[string]bool{
	"name": true, "hosts": true, "vars": true, "environment": true,
	"become": true, "become_user": true, "serial": true, "strategy": true,
	"tasks": true, "handlers": true,
}

func parsePlay(rp map[string]any) (*model.Play, error) {
	p := &model.Play{}
	if v, ok := rp["name"]; ok {
		p.Name = fmt.Sprint(v)
	}
	if v, ok := rp["hosts"]; ok {
		p.Hosts = fmt.Sprint(v)
	}
	if p.Hosts == "" {
		return nil, fmt.Errorf("缺少 hosts")
	}
	if v, ok := rp["vars"]; ok {
		m, err := toAnyMap(v)
		if err != nil {
			return nil, fmt.Errorf("vars: %w", err)
		}
		p.Vars = m
	}
	if v, ok := rp["environment"]; ok {
		m, err := toStringMap(v)
		if err != nil {
			return nil, fmt.Errorf("environment: %w", err)
		}
		p.Environment = m
	}
	if v, ok := rp["become"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("become: %w", err)
		}
		p.Become = b
	}
	if v, ok := rp["become_user"]; ok {
		p.BecomeUser = fmt.Sprint(v)
	}
	if v, ok := rp["serial"]; ok {
		s, err := parseSerial(v)
		if err != nil {
			return nil, fmt.Errorf("serial: %w", err)
		}
		p.Serial = s
	}
	if v, ok := rp["strategy"]; ok {
		st, err := parseStrategy(v)
		if err != nil {
			return nil, fmt.Errorf("strategy: %w", err)
		}
		p.Strategy = st
	}
	if v, ok := rp["tasks"]; ok {
		list, err := toList(v, "tasks")
		if err != nil {
			return nil, err
		}
		for _, it := range list {
			t, err := parseTask(it, false)
			if err != nil {
				return nil, fmt.Errorf("tasks: %w", err)
			}
			p.Tasks = append(p.Tasks, t)
		}
	}
	if v, ok := rp["handlers"]; ok {
		list, err := toList(v, "handlers")
		if err != nil {
			return nil, err
		}
		for _, it := range list {
			t, err := parseTask(it, true)
			if err != nil {
				return nil, fmt.Errorf("handlers: %w", err)
			}
			p.Handlers = append(p.Handlers, t)
		}
	}
	return p, nil
}

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
		return nil, fmt.Errorf("任务必须是 map，实际为 %T", raw)
	}
	t := &model.Task{IsHandler: isHandler}

	if v, ok := m["name"]; ok {
		t.Name = fmt.Sprint(v)
	}
	if v, ok := m["when"]; ok {
		switch x := v.(type) {
		case string:
			t.When = []string{x}
		case []any:
			for _, c := range x {
				t.When = append(t.When, fmt.Sprint(c))
			}
		default:
			return nil, fmt.Errorf("when 仅支持字符串或列表")
		}
	}
	if v, ok := m["loop"]; ok {
		l, err := toAnyList(v, "loop")
		if err != nil {
			return nil, err
		}
		t.Loop = l
	}
	if v, ok := m["with_items"]; ok && t.Loop == nil {
		l, err := toAnyList(v, "with_items")
		if err != nil {
			return nil, err
		}
		t.Loop = l
	}
	if v, ok := m["register"]; ok {
		t.Register = fmt.Sprint(v)
	}
	if v, ok := m["until"]; ok {
		t.Until = fmt.Sprint(v)
	}
	if v, ok := m["notify"]; ok {
		switch x := v.(type) {
		case string:
			t.Notify = []string{x}
		case []any:
			for _, n := range x {
				t.Notify = append(t.Notify, fmt.Sprint(n))
			}
		}
	}
	if v, ok := m["tags"]; ok {
		switch x := v.(type) {
		case string:
			t.Tags = []string{x}
		case []any:
			for _, n := range x {
				t.Tags = append(t.Tags, fmt.Sprint(n))
			}
		}
	}
	if v, ok := m["environment"]; ok {
		env, err := toStringMap(v)
		if err != nil {
			return nil, fmt.Errorf("environment: %w", err)
		}
		t.Environment = env
	}
	if v, ok := m["ignore_errors"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("任务 %q: ignore_errors: %w", t.Label(), err)
		}
		t.IgnoreErrors = b
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
	if v, ok := m["become"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("任务 %q: become: %w", t.Label(), err)
		}
		t.Become = &b
	}
	if v, ok := m["become_user"]; ok {
		t.BecomeUser = fmt.Sprint(v)
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
			return nil, fmt.Errorf("任务 %q: output: %w", t.Label(), err)
		}
	}
	if v, ok := m["no_log"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("任务 %q: no_log: %w", t.Label(), err)
		}
		t.NoLog = b
	}
	if v, ok := m["delegate_to"]; ok {
		t.DelegateTo = fmt.Sprint(v)
	}
	if v, ok := m["run_once"]; ok {
		b, err := model.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("任务 %q: run_once: %w", t.Label(), err)
		}
		t.RunOnce = b
	}
	if v, ok := m["loop_control"]; ok {
		lc, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("任务 %q: loop_control 必须是 map（loop_var: 自定义变量名）", t.Label())
		}
		if lv, ok := lc["loop_var"]; ok {
			t.LoopVar = fmt.Sprint(lv)
			if t.LoopVar == "" {
				return nil, fmt.Errorf("任务 %q: loop_var 不能为空", t.Label())
			}
		}
	}
	if v, ok := m["hook"]; ok {
		t.Hook = fmt.Sprint(v)
		switch t.Hook {
		case "pre_install", "post_install", "pre_uninstall", "post_uninstall":
		default:
			return nil, fmt.Errorf("任务 %q: 不支持的 hook %q（可选: pre_install/post_install/pre_uninstall/post_uninstall）",
				t.Label(), t.Hook)
		}
	}
	// block/rescue/always 任务组（递归解析，支持嵌套）
	for _, key := range []string{"block", "rescue", "always"} {
		v, ok := m[key]
		if !ok {
			continue
		}
		list, err := toList(v, key)
		if err != nil {
			return nil, err
		}
		var tasks []*model.Task
		for _, it := range list {
			bt, err := parseTask(it, isHandler)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
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
	if t.Block != nil {
		t.Module = "block" // 组任务标记（executor 展开执行）
		t.Args = map[string]any{}
		return t, nil
	}
	if t.Rescue != nil || t.Always != nil {
		return nil, fmt.Errorf("任务 %q: rescue/always 必须与 block 同时出现", t.Label())
	}
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
			return nil, fmt.Errorf("任务 %q 同时指定了多个模块（%s、%s）", t.Label(), modName, k)
		}
		modName, modVal = k, v
	}
	if modName == "" {
		return nil, fmt.Errorf("任务 %q 未指定模块", t.Label())
	}
	if modName == "chart" {
		// `chart: <子chart名>` 引用，展开执行子 chart 任务序列
		ref, ok := modVal.(string)
		if !ok || ref == "" {
			return nil, fmt.Errorf("任务 %q: chart 引用必须是子 chart 名字符串", t.Label())
		}
		t.Module = "chart"
		t.ChartRef = ref
		t.Args = map[string]any{}
		return t, nil
	}
	if t.ChartVars != nil {
		return nil, fmt.Errorf("任务 %q: vars 仅用于 chart 引用任务", t.Label())
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
		return nil, fmt.Errorf("模块 %s 的参数必须是字符串或 map", modName)
	}
	if explicitArgs != nil {
		if t.FreeForm != "" {
			return nil, fmt.Errorf("任务 %q: 简写参数与 args 不能同时使用", t.Label())
		}
		for k, v := range explicitArgs {
			t.Args[k] = v
		}
	}
	return t, nil
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
	return fmt.Errorf("无法解析 %q（可用: full/none/oneline/head=N/tail=N）", s)
}

func toStringMap(v any) (map[string]string, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]string, len(x))
		for k, val := range x {
			out[k] = fmt.Sprint(val)
		}
		return out, nil
	case map[string]string:
		return x, nil
	default:
		return nil, fmt.Errorf("需要 map 类型，实际 %T", v)
	}
}

// toAnyMap 转换为 map[string]any。
func toAnyMap(v any) (map[string]any, error) {
	switch x := v.(type) {
	case map[string]any:
		return x, nil
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf("需要 map 类型，实际 %T", v)
	}
}

func toList(v any, what string) ([]any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是列表，实际 %T", what, v)
	}
	return l, nil
}

func toAnyList(v any, what string) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		// 单个字符串项
		return []any{x}, nil
	default:
		return nil, fmt.Errorf("%s 必须是列表，实际 %T", what, v)
	}
}

// parseSerial 解析 serial 表达式：整数 5、百分比 "10%"、或二者逗号列表 "5,10,20"
// （逐批尺寸，最后一个尺寸对剩余主机重复使用）。非法值报错而非静默忽略。
func parseSerial(v any) (string, error) {
	var tokens []string
	switch x := v.(type) {
	case int, float64:
		tokens = []string{fmt.Sprint(x)}
	case string:
		for _, t := range strings.Split(x, ",") {
			tokens = append(tokens, strings.TrimSpace(t))
		}
	case []any:
		for _, it := range x {
			tokens = append(tokens, strings.TrimSpace(fmt.Sprint(it)))
		}
	default:
		return "", fmt.Errorf("需要整数、\"N%%\" 或二者的列表，实际 %T", v)
	}
	for _, t := range tokens {
		if t == "" {
			return "", fmt.Errorf("存在空分批表达式")
		}
		body := strings.TrimSuffix(t, "%")
		if body == "" || strings.Trim(body, "0123456789") != "" {
			return "", fmt.Errorf("无法解析分批表达式 %q（可用: 5 / \"10%%\" / \"5,10,20\"）", t)
		}
	}
	return strings.Join(tokens, ","), nil
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// parseStrategy 解析 play 级 strategy 配置。
func parseStrategy(v any) (*model.Strategy, error) {
	sm, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("必须是 map（type/batch/gate/auto_rollback）")
	}
	st := &model.Strategy{Type: "rolling"}
	if t, ok := sm["type"]; ok {
		st.Type = fmt.Sprint(t)
	}
	switch st.Type {
	case "linear", "rolling", "canary":
	default:
		return nil, fmt.Errorf("不支持的 type %q（可选: linear/rolling/canary）", st.Type)
	}
	if b, ok := sm["batch"]; ok {
		st.Batch = fmt.Sprint(b)
	}
	if ar, ok := sm["auto_rollback"]; ok {
		b, err := model.ParseBool(ar)
		if err != nil {
			return nil, fmt.Errorf("auto_rollback: %w", err)
		}
		st.AutoRollback = b
	}
	if g, ok := sm["gate"]; ok {
		gm, ok := g.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gate 必须是 map（shell/until/retries/delay）")
		}
		gate := &model.Task{Name: "health-gate", Module: "shell"}
		if s, ok := gm["shell"]; ok {
			gate.FreeForm = fmt.Sprint(s)
		}
		if gate.FreeForm == "" {
			return nil, fmt.Errorf("gate 需要 shell（健康检查命令）")
		}
		if u, ok := gm["until"]; ok {
			gate.Until = fmt.Sprint(u)
		}
		if gate.Until == "" {
			// 缺省健康门：命令退出码为 0
			gate.Until = `{{ if eq .result.rc 0 }}ok{{ end }}`
		}
		if r, ok := gm["retries"]; ok {
			gate.Retries = toInt(r)
		}
		if gate.Retries <= 0 {
			gate.Retries = 3
		}
		if d, ok := gm["delay"]; ok {
			gate.DelaySec = toInt(d)
		}
		st.Gate = gate
	}
	return st, nil
}
