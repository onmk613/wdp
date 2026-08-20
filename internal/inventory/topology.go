package inventory

import "wdp/internal/model"

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
