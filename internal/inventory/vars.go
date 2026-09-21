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
// 顺序为拓扑序：任一祖先组先于其后代组（保证 all < 父组 < 子组 的
// 变量与组级连接键叠加方向），同层按字典序稳定输出、group_names 无重复。
// 此前按遍历到达顺序 + 重复移尾，菱形嵌套（parent→{a,b}，主机同属
// a、b）下父组会被排到子组之后，父组变量/连接键静默覆盖子组。
func (inv *Inventory) groupMembership() map[string][]string {
	// 第一步：host → 组集合（环安全的递归展开；父组沿 children 下沉到
	// 全部后代主机的集合中）
	sets := map[string]map[string]bool{}
	var walk func(group string, seen map[string]bool, chain []string)
	walk = func(group string, seen map[string]bool, chain []string) {
		if seen[group] {
			return
		}
		seen[group] = true
		grp, ok := inv.Groups[group]
		if !ok {
			return
		}
		full := append(append([]string{}, chain...), group)
		for _, hname := range grp.HostNames {
			if sets[hname] == nil {
				sets[hname] = map[string]bool{}
			}
			for _, g := range full {
				sets[hname][g] = true
			}
		}
		for _, c := range grp.Children {
			walk(c, seen, full)
		}
	}
	names := make([]string, 0, len(inv.Groups))
	for n := range inv.Groups {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		walk(n, map[string]bool{}, nil)
	}

	// 第二步：组间祖先关系（memo + 占位防环）
	parents := map[string][]string{}
	for pn, pg := range inv.Groups {
		for _, c := range pg.Children {
			parents[c] = append(parents[c], pn)
		}
	}
	ancMemo := map[string]map[string]bool{}
	var ancOf func(g string) map[string]bool
	ancOf = func(g string) map[string]bool {
		if m, ok := ancMemo[g]; ok {
			return m
		}
		m := map[string]bool{}
		ancMemo[g] = m // 先占位：children 环不致无限递归
		for _, p := range parents[g] {
			if p == g {
				continue
			}
			m[p] = true
			maps.Copy(m, ancOf(p))
		}
		return m
	}

	// 第三步：每台主机的组集合按拓扑序展开（祖先先行，同层字典序）
	membership := make(map[string][]string, len(sets))
	for hname, set := range sets {
		list := make([]string, 0, len(set))
		for g := range set {
			list = append(list, g)
		}
		slices.Sort(list)
		ordered := make([]string, 0, len(list))
		emitted := map[string]bool{}
		for len(list) > 0 {
			progressed := false
			for i, g := range list {
				ready := true
				for a := range ancOf(g) {
					if set[a] && !emitted[a] {
						ready = false
						break
					}
				}
				if ready {
					ordered = append(ordered, g)
					emitted[g] = true
					list = append(list[:i], list[i+1:]...)
					progressed = true
					break
				}
			}
			if !progressed { // 组环兜底：剩余按字典序清空，不死循环
				ordered = append(ordered, list...)
				break
			}
		}
		membership[hname] = ordered
	}
	return membership
}
