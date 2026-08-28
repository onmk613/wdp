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

// Parse 解析 playbook 内容。play 级简单键经 model.Play 的 yaml tag 直接
// 映射（新增简单字段只需在 model 打 tag），特殊语义键由 parsePlayNode
// 抽取节点手工解析。
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
	for i, item := range seq.Content {
		p, err := parsePlayNode(item)
		if err != nil {
			return nil, fmt.Errorf("play #%d: %w", i+1, err)
		}
		plays = append(plays, p)
	}
	return plays, nil
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
	if p.Hosts == "" {
		return nil, errors.New("missing hosts")
	}
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
func parseTaskList(n *yaml.Node, isHandler bool) ([]*model.Task, error) {
	if n == nil {
		return nil, nil
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

// nodeValue 把节点解码回 any（手工解析路径的输入形态）。
func nodeValue(n *yaml.Node) (any, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
