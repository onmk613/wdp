package inventory

import (
	"maps"
	"slices"
)

// applyVars 为每台主机计算最终变量：all < 父组 < 子组 < 主机。
// 连接参数键（conn/user/port 等）不进入变量域——组级的已在 buildHost
// 阶段提取为连接参数，条目级的从来只做参数。
func (inv *Inventory) applyVars() {
	membership := inv.groupMembership()
	for _, h := range inv.Hosts {
		merged := map[string]any{}
		for k, v := range inv.AllVars {
			if !isHostKey(k) {
				merged[k] = v
			}
		}
		for _, g := range membership[h.Name] {
			for k, v := range inv.Groups[g].Vars {
				if !isHostKey(k) {
					merged[k] = v
				}
			}
		}
		maps.Copy(merged, h.Vars)
		merged["inventory_hostname"] = h.Name
		groups := append([]string{}, membership[h.Name]...)
		slices.Sort(groups)
		merged["group_names"] = groups
		h.Vars = merged
	}
}

// groupMembership 计算每台主机的有序组归属链（含 children 递归展开）。
// 追加去重：同一组经不同路径多次到达时移到末尾（后写优先），
// 保持父→子的变量叠加顺序且 group_names 无重复。
func (inv *Inventory) groupMembership() map[string][]string {
	membership := map[string][]string{} // host → 有序组名列表
	appendUnique := func(list []string, items []string) []string {
		for _, g := range items {
			for i, x := range list {
				if x == g {
					list = append(list[:i], list[i+1:]...)
					break
				}
			}
			list = append(list, g)
		}
		return list
	}
	var walk func(group string, chain []string)
	visited := map[string]bool{}
	walk = func(group string, chain []string) {
		if visited[group] {
			return
		}
		visited[group] = true
		grp, ok := inv.Groups[group]
		if !ok {
			return
		}
		cur := append(append([]string{}, chain...), group)
		for _, hname := range grp.HostNames {
			membership[hname] = appendUnique(membership[hname], cur)
		}
		for _, c := range grp.Children {
			walk(c, cur)
		}
	}
	names := make([]string, 0, len(inv.Groups))
	for n := range inv.Groups {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		visited = map[string]bool{}
		walk(n, nil)
	}
	return membership
}
