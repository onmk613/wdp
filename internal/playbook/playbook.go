// Package playbook 解析声明式的 playbook YAML。
// 每个任务是单键 map：已知控制属性之外的唯一一个键即模块名。
package playbook

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"wdp/internal/i18n"
	"wdp/internal/model"
)

// Load 从文件解析 playbook。
func Load(path string) ([]*model.Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(i18n.T("failed to read playbook: %w", "读取 playbook 失败: %w"), err)
	}
	plays, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for _, p := range plays {
		for _, t := range append(append([]*model.Task{}, p.Tasks...), p.Handlers...) {
			if t.Module == "" {
				return nil, fmt.Errorf(i18n.T("play %s has a task without a specified module", "play %s 存在未指定模块的任务"), p.Name)
			}
		}
	}
	return plays, nil
}

// Parse 解析 playbook 内容。
func Parse(data []byte) ([]*model.Play, error) {
	var raw []map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf(i18n.T("failed to parse playbook: %w", "解析 playbook 失败: %w"), err)
	}
	plays := make([]*model.Play, 0, len(raw))
	for i, rp := range raw {
		p, err := parsePlay(rp)
		if err != nil {
			return nil, fmt.Errorf(i18n.T("play #%d: %w", "第 %d 个 play: %w"), i+1, err)
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
		return nil, errors.New(i18n.T("missing hosts", "缺少 hosts"))
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
