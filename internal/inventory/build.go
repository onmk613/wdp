package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"wdp/internal/model"
)

// build 由原始结构构建 Inventory：排序保证确定性，挂接 group_vars/host_vars 后计算变量域。
func build(raw rawInventory, varDirs []string) (*Inventory, error) {

	inv := &Inventory{
		Groups:  map[string]*model.Group{},
		AllVars: map[string]any{},
	}

	// all 组
	if all, ok := raw["all"]; ok {
		for k, v := range all.Vars {
			inv.AllVars[k] = v
		}
	}

	// 按组名排序遍历（all 最先），保证同一主机在多个组定义时
	// 参数合并结果确定：后处理的组（字典序）覆盖先处理的组
	groupNames := make([]string, 0, len(raw))
	for gname := range raw {
		groupNames = append(groupNames, gname)
	}
	sort.Strings(groupNames)

	hostRaw := map[string]map[string]any{} // 主机名 → 合并后的原始参数
	hostIndex := map[string]*model.Host{}
	for _, gname := range groupNames {
		g := raw[gname]
		if gname == "all" {
			for hname, hvars := range g.Hosts {
				mergeHostRaw(hostRaw, hname, hvars)
			}
			continue
		}
		grp := &model.Group{Name: gname, Vars: g.Vars, Children: g.Children}
		hnames := make([]string, 0, len(g.Hosts))
		for hname := range g.Hosts {
			hnames = append(hnames, hname)
		}
		sort.Strings(hnames)
		for _, hname := range hnames {
			mergeHostRaw(hostRaw, hname, g.Hosts[hname])
			grp.HostNames = append(grp.HostNames, hname)
		}
		inv.Groups[gname] = grp
	}
	// 构建主机对象并回填组成员
	inv.Hosts = make([]*model.Host, 0, len(hostRaw))
	for hname, hvars := range hostRaw {
		h, err := buildHost(hname, hvars)
		if err != nil {
			return nil, fmt.Errorf("主机 %s: %w", hname, err)
		}
		hostIndex[hname] = h
		inv.Hosts = append(inv.Hosts, h)
	}
	for _, grp := range inv.Groups {
		for _, hname := range grp.HostNames {
			grp.Hosts = append(grp.Hosts, hostIndex[hname])
		}
	}

	// 校验 children 引用
	for _, grp := range inv.Groups {
		for _, c := range grp.Children {
			if _, ok := inv.Groups[c]; !ok {
				return nil, fmt.Errorf("组 %s 引用了不存在的子组 %s", grp.Name, c)
			}
		}
	}

	// 约定变量目录（按传入顺序，后者覆盖）：group_vars/<组>.yaml、host_vars/<主机>.yaml
	for _, dir := range varDirs {
		if err := inv.loadVarDirs(dir); err != nil {
			return nil, err
		}
	}

	sort.Slice(inv.Hosts, func(i, j int) bool { return inv.Hosts[i].Name < inv.Hosts[j].Name })
	inv.applyVars(hostIndex)
	inv.precomputeTopology()
	return inv, nil
}

// loadVarDirs 加载目录约定变量。
func (inv *Inventory) loadVarDirs(dir string) error {
	if err := inv.loadGroupVars(filepath.Join(dir, "group_vars")); err != nil {
		return err
	}
	return inv.loadHostVars(filepath.Join(dir, "host_vars"))
}

// loadGroupVars 加载 group_vars/<组名>.yaml（all.yaml 合入全局变量；
// 文件对应组不存在时忽略——组在其它 inventory 中定义的场景已由合并阶段处理）。
func (inv *Inventory) loadGroupVars(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isYAMLName(e.Name()) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		vars, err := readVarsYAML(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("group_vars/%s: %w", e.Name(), err)
		}
		if name == "all" {
			inv.AllVars = mergeVars(inv.AllVars, vars)
			continue
		}
		if g, ok := inv.Groups[name]; ok {
			g.Vars = mergeVars(g.Vars, vars)
		}
	}
	return nil
}

// loadHostVars 加载 host_vars/<主机名>.yaml（合并进主机原始变量）。
func (inv *Inventory) loadHostVars(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", dir, err)
	}
	byName := map[string]*model.Host{}
	for _, h := range inv.Hosts {
		byName[h.Name] = h
	}
	for _, e := range entries {
		if e.IsDir() || !isYAMLName(e.Name()) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		h, ok := byName[name]
		if !ok {
			continue
		}
		vars, err := readVarsYAML(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("host_vars/%s: %w", e.Name(), err)
		}
		h.Vars = mergeVars(h.Vars, vars)
	}
	return nil
}

func isYAMLName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func readVarsYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vars map[string]any
	if err := yaml.Unmarshal(data, &vars); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	return vars, nil
}

// mergeHostRaw 将主机条目参数合并进累积表（同主机多处定义：后处理的组覆盖）。
func mergeHostRaw(hostRaw map[string]map[string]any, name string, vars map[string]any) {
	if _, ok := hostRaw[name]; !ok {
		hostRaw[name] = map[string]any{}
	}
	for k, v := range vars {
		hostRaw[name][k] = v
	}
}
