package executor

// 内置变量的单一事实来源。
//
// 此前"顶层 play 注入"（batch.go 内联）与"子 chart 作用域穿透"（expand.go
// 清单）是两处平行维护的代码——新增内置变量必须同时改两处，漏一处即产生
// 顶层与子 chart 语义漂移。现在：名字清单与赋值逻辑都集中在这里，
// 新增内置变量只改本文件一处，两条路径自动一致。

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"

	"wdp/internal/model"
)

// builtinVarNames 是强制注入的内置变量（不可被任何静态层/运行时层覆盖，
// 子 chart 作用域穿透）。新增变量：在此登记 + 在 injectBuiltins 赋值。
var builtinVarNames = []string{
	"inventory_hostname", // 自己是谁
	"group_names",        // 自己所属组
	"play_hosts",         // 当前 play 全部选中主机
	"play_batch",         // 当前批次主机
	"groups",             // 组名→成员（含 children 展开、group_by/add_host 动态组）
	"hosts",              // 主机名→{name,address,port,conn}
	"hostvars",           // 主机名→该主机变量域（inventory 变量 + fact store）
	"playbook_dir",       // playbook/chart 根目录绝对路径（Ansible 同名语义）
}

// injectBuiltins 在变量域组装完成后强制注入内置变量（最后写入，覆盖一切）。
// hostvars 由调用方每批次计算一次后传入：该快照遍历全部主机与 fact store，
// 逐主机重算在千台规模下是 O(N²)；与 hostvarsSnapshot 注释承诺的"批内共享"
// 语义也一致——同批次内后写 facts 对先前主机不可见。
func (e *Executor) injectBuiltins(vars map[string]any, h *model.Host, playHosts, batchHosts []*model.Host, hostvars map[string]map[string]any) {
	vars["inventory_hostname"] = h.Name
	// group_names 的权威来源是 inventory 层写入的 h.Vars（含运行期 add_host 更新）
	vars["group_names"] = h.Vars["group_names"]
	vars["play_hosts"] = hostNames(playHosts)
	vars["play_batch"] = hostNames(batchHosts)
	vars["groups"] = e.Inv.GroupsMap()
	vars["hosts"] = e.Inv.HostsMeta()
	vars["hostvars"] = hostvars
	vars["playbook_dir"] = e.Opts.BaseDir
}

// injectPlanBuiltins 是 plan 执行模式的内置变量注入：只覆盖执行期才能
// 确定的 playbook_dir（物化目录路径）与批次事实；跨主机信息（groups/
// hosts/hostvars/group_names/inventory_hostname）保留编译期冻结值——
// 主机侧执行时拿不到其他主机的 facts（docs/15 §2.3）。
func (e *Executor) injectPlanBuiltins(vars map[string]any, h *model.Host, playHosts, batchHosts []*model.Host) {
	vars["play_hosts"] = hostNames(playHosts)
	vars["play_batch"] = hostNames(batchHosts)
	vars["playbook_dir"] = e.Opts.BaseDir
}

// hostvarsSnapshot 返回主机名→变量域快照：inventory 主机变量 + fact store
// （setup 采集 / set_fact 写入 / stat 探测）。register 结果不进入快照——
// 跨主机共享一律经 set_fact（边界清晰：register 本机域，set_fact 事实库）。
//
// 快照在批次组装时生成一次、批内共享：同批次内先执行主机写入的 facts
// 对后执行主机不可见（与分批语义一致），下一批次/play 可见——两段式
// 编排（收集 play → 配置 play）不受影响。
func (e *Executor) hostvarsSnapshot() map[string]map[string]any {
	e.factsMu.Lock()
	defer e.factsMu.Unlock()
	out := make(map[string]map[string]any, len(e.Inv.Hosts))
	for _, h := range e.Inv.Hosts {
		m := make(map[string]any, len(h.Vars)+8)
		maps.Copy(m, h.Vars)
		if f := e.facts[h.Name]; len(f) > 0 {
			maps.Copy(m, f)
		}
		out[h.Name] = m
	}
	return out
}

// ---- fact cache（跨运行的 fact 持久化，--fact-cache PATH 启用） ----

// loadFactCache 把 JSON fact cache 并入 fact store（运行期写入后到自然覆盖）。
// cache 损坏不阻塞部署：告警后忽略（cache 是加速器，不是数据源）。
func (e *Executor) loadFactCache(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 不存在是常态（首次运行）
	}
	var cached map[string]map[string]any
	if err := json.Unmarshal(data, &cached); err != nil {
		e.Rep.PlayMsg("fact cache %s is corrupt (%v), ignoring", path, err)
		return
	}
	e.factsMu.Lock()
	defer e.factsMu.Unlock()
	for host, facts := range cached {
		cur := e.facts[host]
		if cur == nil {
			cur = map[string]any{}
			e.facts[host] = cur
		}
		maps.Copy(cur, facts)
	}
}

// saveFactCache 原子落盘 fact store（临时文件 + rename；0600——facts 可能
// 含敏感数据）。best-effort：失败仅告警，不影响部署结果。
func (e *Executor) saveFactCache(path string) {
	e.factsMu.Lock()
	snapshot := make(map[string]map[string]any, len(e.facts))
	for host, facts := range e.facts {
		snapshot[host] = maps.Clone(facts)
	}
	e.factsMu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		e.Rep.PlayMsg("fact cache save failed: %v", err)
		return
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		e.Rep.PlayMsg("fact cache save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		e.Rep.PlayMsg("fact cache save failed: %v", err)
	}
}
