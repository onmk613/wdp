// Package playbook 解析声明式的 playbook YAML。
// 每个任务是单键 map：已知控制属性之外的唯一一个键即模块名。
package playbook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"wdp/internal/model"
)

// Load 从文件解析 playbook（含 include 片段静态展开）。
func Load(path string) ([]*model.Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read playbook: %w", err)
	}
	plays, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := expandIncludes(plays, filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("failed to parse playbook: %w", err)
	}
	for _, p := range plays {
		for _, t := range append(append([]*model.Task{}, p.Tasks...), p.Handlers...) {
			if t.Module == "" {
				return nil, fmt.Errorf("play %s has a task without a specified module", p.Name)
			}
		}
	}
	return plays, nil
}

// Parse 解析 playbook 内容。两种形态（可混用，裸任务按出现顺序合并为
// 一个隐式 play）：
//
//	play 形态：- name: x / hosts: y / tasks: [...]（完整编排能力）
//	裸任务形态：- name: x / shell: ...（直接任务列表——简单相位只关心
//	任务本身；hosts 留空由执行侧决定：chart 的 CLI 路径填 chart 同名
//	组，web 路径打选择器全集）
//
// play 级简单键经 model.Play 的 yaml tag 直接映射（新增简单字段只需在
// model 打 tag），特殊语义键由 parsePlayNode 抽取节点手工解析。
func Parse(data []byte) ([]*model.Play, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse playbook: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	seq := doc.Content[0]
	if seq.Kind == yaml.ScalarNode && seq.Tag == "!!null" {
		return nil, nil // 空文档
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, errors.New("playbook must be a list of plays")
	}
	plays := make([]*model.Play, 0, len(seq.Content))
	var implicit *model.Play // 连续裸任务合并进的隐式 play
	for i, item := range seq.Content {
		if itemIsBareTask(item) {
			var m map[string]any
			if err := item.Decode(&m); err != nil {
				return nil, fmt.Errorf("task #%d (line %d): %w", i+1, item.Line, err)
			}
			t, err := parseTask(m, false)
			if err != nil {
				return nil, fmt.Errorf("task #%d (line %d): %w", i+1, item.Line, err)
			}
			t.Line = item.Line
			if implicit == nil {
				implicit = &model.Play{}
				plays = append(plays, implicit)
			}
			implicit.Tasks = append(implicit.Tasks, t)
			continue
		}
		implicit = nil
		p, err := parsePlayNode(item)
		if err != nil {
			return nil, fmt.Errorf("play #%d (line %d): %w", i+1, item.Line, err)
		}
		plays = append(plays, p)
	}
	return plays, nil
}

// itemIsBareTask 序列项是否裸任务形态：map 里出现模块键（既不是任务
// 控制键也不是 play 键）即任务；出现 play 专属键（tasks/handlers/serial/
// strategy/hosts）即 play。两者都无（如只有 name:）按 play 处理（现状）。
func itemIsBareTask(n *yaml.Node) bool {
	if n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if k == "tasks" || k == "handlers" || k == "serial" || k == "strategy" || k == "hosts" {
			return false
		}
		if !taskKeys[k] && !playKeys[k] {
			return true // 模块键
		}
	}
	return false
}

// playKeys 是 play 级已知键——供任务级模块键排除（任务 map 中出现这些键
// 不当作模块名）。键集 = 手工解析特殊键 + model.Play 的 yaml tag 键，
// 漂移由 TestPlayKeysCoverModelTags 反射锁定。
var playKeys = map[string]bool{
	"name": true, "hosts": true, "vars": true, "environment": true,
	"become": true, "become_user": true, "serial": true, "strategy": true,
	"tasks": true, "handlers": true,
}

// parsePlayNode 半结构化解析单个 play 节点：
//   - become/serial/strategy/tasks/handlers 抽取节点手工解析（宽容布尔、
//     批次表达式校验、策略默认值、单键 map 模块语法）；
//   - 其余键就地摘除后经 yaml tag 直接 Decode 进 model.Play。
func parsePlayNode(n *yaml.Node) (*model.Play, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("play must be a map, got %s", n.Tag)
	}
	var becomeN, serialN, strategyN, tasksN, handlersN *yaml.Node
	kept := n.Content[:0]
	for i := 0; i+1 < len(n.Content); i += 2 {
		val := n.Content[i+1]
		switch n.Content[i].Value {
		case "become":
			becomeN = val
		case "serial":
			serialN = val
		case "strategy":
			strategyN = val
		case "tasks":
			tasksN = val
		case "handlers":
			handlersN = val
		default:
			kept = append(kept, n.Content[i], val)
		}
	}
	n.Content = kept
	p := &model.Play{}
	if err := n.Decode(p); err != nil {
		return nil, err
	}
	// hosts 可选：裸任务相位/简单 play 不写 hosts（空值），由执行侧
	// 缺省（chart 的 CLI 路径 = chart 同名组；web 路径 = 选择器全集；
	// executor 对空模式按 all 处理）
	if becomeN != nil {
		v, err := nodeValue(becomeN)
		if err != nil {
			return nil, fmt.Errorf("become: %w", err)
		}
		b, err := model.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("become: %w", err)
		}
		p.Become = b
	}
	if serialN != nil {
		v, err := nodeValue(serialN)
		if err != nil {
			return nil, fmt.Errorf("serial: %w", err)
		}
		if p.Serial, err = parseSerial(v); err != nil {
			return nil, fmt.Errorf("serial: %w", err)
		}
	}
	if strategyN != nil {
		v, err := nodeValue(strategyN)
		if err != nil {
			return nil, fmt.Errorf("strategy: %w", err)
		}
		if p.Strategy, err = parseStrategy(v); err != nil {
			return nil, fmt.Errorf("strategy: %w", err)
		}
	}
	var err error
	if p.Tasks, err = parseTaskList(tasksN, false); err != nil {
		return nil, fmt.Errorf("tasks: %w", err)
	}
	if p.Handlers, err = parseTaskList(handlersN, true); err != nil {
		return nil, fmt.Errorf("handlers: %w", err)
	}
	return p, nil
}

// parseTaskList 解析 tasks/handlers 节点为任务列表（递归入口 parseTask）。
// 序列项的行号写入 Task.Line（lint 问题定位），解析错误也带行号。
func parseTaskList(n *yaml.Node, isHandler bool) ([]*model.Task, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind == yaml.SequenceNode {
		var tasks []*model.Task
		for _, item := range n.Content {
			t, err := parseTaskNode(item, isHandler)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, t)
		}
		return tasks, nil
	}
	v, err := nodeValue(n)
	if err != nil {
		return nil, err
	}
	list, err := toList(v, "tasks")
	if err != nil {
		return nil, err
	}
	var tasks []*model.Task
	for _, it := range list {
		t, err := parseTask(it, isHandler)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// parseTaskNode 解析单个序列项（带行号）；非 map 形态（简写任务）也走这里。
func parseTaskNode(n *yaml.Node, isHandler bool) (*model.Task, error) {
	if n.Kind == yaml.MappingNode {
		var m map[string]any
		if err := n.Decode(&m); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		t, err := parseTask(m, isHandler)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		t.Line = n.Line
		return t, nil
	}
	// 简写等非 map 形态：走 any 解析（行号信息不可靠，置 0）
	v, err := nodeValue(n)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	t, err := parseTask(v, isHandler)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	return t, nil
}

// nodeValue 把节点解码回 any（手工解析路径的输入形态）。
func nodeValue(n *yaml.Node) (any, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
