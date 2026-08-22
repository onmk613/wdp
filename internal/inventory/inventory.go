// Package inventory 解析 YAML 主机清单：组、组间父子、组变量、主机变量。
//
//	all:
//	  vars: {ssh_user: root}
//	webservers:
//	  hosts:
//	    web1: {host: 10.0.0.11, port: 22}
//	    web2: {conn: agent, agent_url: 'http://10.0.0.12:7602'}
//	  vars: {nginx_port: 8080}
//	  children: [frontend]
package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"wdp/internal/config"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// Inventory 是解析后的主机清单。
type Inventory struct {
	Hosts   []*model.Host
	Groups  map[string]*model.Group
	AllVars map[string]any

	groupMap  map[string][]string       // 组名 → 成员主机名（含 children 展开；all=全部）
	hostsMeta map[string]map[string]any // 主机名 → {name,address,port,conn}
}

type rawGroup struct {
	Hosts    map[string]map[string]any `yaml:"hosts"`
	Vars     map[string]any            `yaml:"vars"`
	Children []string                  `yaml:"children"`
}

type rawInventory map[string]rawGroup

// Load 从文件解析（自动加载同目录 group_vars/ 与 host_vars/ 约定变量）。
// 使用内置默认连接参数（不读 wdp.cfg；组合根用 LoadWithConfig 显式注入）。
func Load(path string) (*Inventory, error) {
	return LoadWithConfig(path, nil)
}

// LoadWithConfig 同 Load，但以显式配置提供主机条目的默认连接参数。
func LoadWithConfig(path string, cfg *config.Config) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(i18n.T("failed to read inventory: %w", "读取 inventory 失败: %w"), err)
	}
	return parseOne(data, []string{filepath.Dir(path)}, cfg)
}

// LoadMerge 加载并合并多个 inventory 文件（-i 可重复指定）：
// 同名组合并（主机参数后者覆盖、组变量深合并、children 并集），
// 各文件同目录的 group_vars/host_vars 均按序加载（后者覆盖）。
// 使用内置默认连接参数；组合根用 LoadMergeWithConfig 显式注入 wdp.cfg。
func LoadMerge(paths []string) (*Inventory, error) {
	return LoadMergeWithConfig(paths, nil)
}

// LoadMergeWithConfig 同 LoadMerge，但以显式配置提供主机条目的默认连接参数。
func LoadMergeWithConfig(paths []string, cfg *config.Config) (*Inventory, error) {
	if len(paths) == 0 {
		return nil, errors.New(i18n.T("no inventory file specified", "未指定 inventory 文件"))
	}
	if len(paths) == 1 {
		return LoadWithConfig(paths[0], cfg)
	}
	merged := rawInventory{}
	var dirs []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf(i18n.T("failed to read inventory %s: %w", "读取 inventory %s 失败: %w"), p, err)
		}
		var raw rawInventory
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf(i18n.T("failed to parse inventory %s: %w", "解析 inventory %s 失败: %w"), p, err)
		}
		merged = mergeRaw(merged, raw)
		dirs = append(dirs, filepath.Dir(p))
	}
	return build(merged, dirs, cfg)
}

// mergeRaw 合并两份原始 inventory（b 覆盖 a）。
func mergeRaw(a, b rawInventory) rawInventory {
	out := rawInventory{}
	for k, g := range a {
		out[k] = g
	}
	for name, g := range b {
		cur, ok := out[name]
		if !ok {
			out[name] = g
			continue
		}
		if cur.Hosts == nil {
			cur.Hosts = g.Hosts
		} else if g.Hosts != nil {
			for hn, hv := range g.Hosts {
				if old := cur.Hosts[hn]; old != nil {
					mergedHost := map[string]any{}
					for k, v := range old {
						mergedHost[k] = v
					}
					for k, v := range hv {
						mergedHost[k] = v
					}
					cur.Hosts[hn] = mergedHost
				} else {
					cur.Hosts[hn] = hv
				}
			}
		}
		cur.Vars = mergeVars(cur.Vars, g.Vars)
		if len(g.Children) > 0 {
			seen := map[string]bool{}
			for _, c := range cur.Children {
				seen[c] = true
			}
			for _, c := range g.Children {
				if !seen[c] {
					cur.Children = append(cur.Children, c)
				}
			}
		}
		out[name] = cur
	}
	return out
}

// mergeVars 深合并变量 map（b 覆盖 a；嵌套 map 递归，标量与列表整体替换）。
func mergeVars(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if am, ok := out[k].(map[string]any); ok {
			if bm, ok := v.(map[string]any); ok {
				out[k] = mergeVars(am, bm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// Parse 解析 YAML 内容（不加载 group_vars/host_vars 目录；测试与内联构造用，
// 连接参数取内置默认）。需要 wdp.cfg 默认值时用 ParseWithConfig。
func Parse(data []byte) (*Inventory, error) {
	return parseOne(data, nil, nil)
}

// ParseWithConfig 同 Parse，但以显式配置提供主机条目的默认连接参数。
func ParseWithConfig(data []byte, cfg *config.Config) (*Inventory, error) {
	return parseOne(data, nil, cfg)
}

func parseOne(data []byte, varDirs []string, cfg *config.Config) (*Inventory, error) {
	var raw rawInventory
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf(i18n.T("failed to parse inventory: %w", "解析 inventory 失败: %w"), err)
	}
	return build(raw, varDirs, cfg)
}
