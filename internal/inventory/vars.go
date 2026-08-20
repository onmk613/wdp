package inventory

import (
	"sort"

	"wdp/internal/model"
)

// applyVars 为每台主机计算最终变量：all < 父组 < 子组 < 主机。
func (inv *Inventory) applyVars(hostIndex map[string]*model.Host) {
	// 展开组归属（含 children 递归）。追加去重：同一组经不同路径多次到达时
	// 移到末尾（后写优先），保持父→子的变量叠加顺序且 group_names 无重复。
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
		for _, h := range grp.Hosts {
			membership[h.Name] = appendUnique(membership[h.Name], cur)
		}
		for _, c := range grp.Children {
			walk(c, cur)
		}
	}
	names := make([]string, 0, len(inv.Groups))
	for n := range inv.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		visited = map[string]bool{}
		walk(n, nil)
	}

	for _, h := range inv.Hosts {
		merged := map[string]any{}
		for k, v := range inv.AllVars {
			merged[k] = v
		}
		for _, g := range membership[h.Name] {
			for k, v := range inv.Groups[g].Vars {
				merged[k] = v
			}
		}
		for k, v := range h.Vars {
			merged[k] = v
		}
		merged["inventory_hostname"] = h.Name
		groups := append([]string{}, membership[h.Name]...)
		sort.Strings(groups)
		merged["group_names"] = groups
		h.Vars = merged
	}
}
