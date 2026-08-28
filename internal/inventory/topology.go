package inventory

import (
	"maps"
	"slices"

	"wdp/internal/model"
)

// precomputeTopology 预计算组拓扑与主机元信息（内置变量 groups/hosts 的数据源）。
func (inv *Inventory) precomputeTopology() {
	inv.groupMap = map[string][]string{"all": hostNames(inv.Hosts)}
	for name, g := range inv.Groups {
		inv.groupMap[name] = expandGroup(inv, g, map[string]bool{})
	}
	inv.hostsMeta = make(map[string]map[string]any, len(inv.Hosts))
	for _, h := range inv.Hosts {
		inv.hostsMeta[h.Name] = map[string]any{
			"name": h.Name, "address": h.Address, "port": h.Port, "conn": h.Conn,
		}
	}
}

// GroupsMap 返回组名→成员主机名（含 children 展开；all = 全部主机）。
func (inv *Inventory) GroupsMap() map[string][]string { return inv.groupMap }

// HostsMeta 返回主机名→{name,address,port,conn}。
func (inv *Inventory) HostsMeta() map[string]map[string]any { return inv.hostsMeta }

// HostByName 按名查找主机（delegate_to 等场景；未找到返回 nil）。
func (inv *Inventory) HostByName(name string) *model.Host {
	for _, h := range inv.Hosts {
		if h.Name == name {
			return h
		}
	}
	return nil
}

// AddDynamicGroup 运行时（group_by 模块）添加动态组：主机加入组并重建拓扑缓存。
// 后续 play 的 hosts 选择与 .groups 内置变量即可引用该组。
func (inv *Inventory) AddDynamicGroup(name string, members []string) {
	grp, ok := inv.Groups[name]
	if !ok {
		grp = &model.Group{Name: name}
		inv.Groups[name] = grp
	}
	seen := map[string]bool{}
	for _, h := range grp.Hosts {
		seen[h.Name] = true
	}
	for _, m := range members {
		if seen[m] {
			continue
		}
		if h := inv.HostByName(m); h != nil {
			grp.Hosts = append(grp.Hosts, h)
			grp.HostNames = append(grp.HostNames, m)
			seen[m] = true
		}
	}
	inv.precomputeTopology()
}

// AddRuntimeHost 运行时（add_host 模块）新增或更新主机，可同时入组。
// 已存在同名主机时仅更新地址/端口/连接字段与变量（不重复入列）；
// 新主机写入 group_names，保证 group_names 内置变量口径与静态 inventory 一致。
// 后续 play 的 hosts 选择与 groups/hosts/hostvars 内置变量即刻可见。
func (inv *Inventory) AddRuntimeHost(h *model.Host, groups []string) {
	if h == nil || h.Name == "" {
		return
	}
	if h.Vars == nil {
		h.Vars = map[string]any{}
	}
	if existing := inv.HostByName(h.Name); existing != nil {
		if h.Address != "" {
			existing.Address = h.Address
		}
		if h.Port != 0 {
			existing.Port = h.Port
		}
		if h.Conn != "" {
			existing.Conn = h.Conn
		}
		if h.AgentURL != "" {
			existing.AgentURL = h.AgentURL
		}
		if h.User != "" {
			existing.User = h.User
		}
		maps.Copy(existing.Vars, h.Vars)
		h = existing
	} else {
		if h.Address == "" {
			h.Address = h.Name
		}
		inv.Hosts = append(inv.Hosts, h)
	}
	for _, g := range groups {
		inv.AddDynamicGroup(g, []string{h.Name})
	}
	if len(groups) == 0 {
		inv.precomputeTopology()
		return
	}
	// group_names 与静态 inventory 构建口径一致：主机变量里保留所属组清单
	names, _ := h.Vars["group_names"].([]string)
	for _, g := range groups {
		if !slices.Contains(names, g) {
			names = append(names, g)
		}
	}
	h.Vars["group_names"] = names
}

func hostNames(hosts []*model.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

// expandGroup 递归展开组的全部成员主机名（含 children；seen 防环）。
func expandGroup(inv *Inventory, g *model.Group, seen map[string]bool) []string {
	if seen[g.Name] {
		return nil
	}
	seen[g.Name] = true
	out := make([]string, 0, len(g.Hosts))
	for _, h := range g.Hosts {
		out = append(out, h.Name)
	}
	for _, c := range g.Children {
		if cg, ok := inv.Groups[c]; ok {
			out = append(out, expandGroup(inv, cg, seen)...)
		}
	}
	return out
}
