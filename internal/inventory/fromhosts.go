package inventory

// FromHosts 用运行期构造的主机列表组装最小 inventory（`wdp run --hosts`
// 内联主机模式）：不经过 YAML 文件，全部主机构成 all（无命名组），
// group_names 注入 "all"，拓扑缓存即刻可用——Select("all")、内置变量
// groups/hosts/hostvars 与文件清单行为一致。

import "wdp/internal/model"

// FromHosts 由主机列表构造内联 inventory（去重、保持传入顺序）。
func FromHosts(hosts []*model.Host) *Inventory {
	inv := &Inventory{
		Groups:  map[string]*model.Group{},
		AllVars: map[string]any{},
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		if h == nil || h.Name == "" || seen[h.Name] {
			continue
		}
		seen[h.Name] = true
		if h.Vars == nil {
			h.Vars = map[string]any{}
		}
		// 与文件清单同口径：主机变量域带 group_names（内置变量注入读取此处）
		h.Vars["group_names"] = []string{"all"}
		h.Vars["inventory_hostname"] = h.Name
		inv.Hosts = append(inv.Hosts, h)
	}
	inv.precomputeTopology()
	return inv
}
