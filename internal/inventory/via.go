package inventory

// via 中继声明（docs/15 §7.4）：组/主机变量 via: <主机名> 声明可达性——
// 控制端对 via 根（跳板机）下发整个网段的 plan 分片，跳板机上的 agent
// 作为该网段的本地控制端执行。与 delegate_to 的区别：delegate_to 是换
// 一台机器执行单个任务，仍要求控制端直连被委托机；via 是传递可达。
//
//	prod-inner:
//	  vars:
//	    via: bastion-1   # 段内主机经 bastion-1 可达；bastion-1 自身也可有 via（链式）

import (
	"fmt"
	"strings"
)

// ViaOf 返回主机声明的 via 中继（未声明返回空串）。
func (inv *Inventory) ViaOf(host string) string {
	h := inv.HostByName(host)
	if h == nil {
		return ""
	}
	v, _ := h.Vars["via"].(string)
	return v
}

// ViaChain 返回主机的完整中继链（从第一跳到根，不含主机自身）。
// 链上的环与未知名由 ValidateVia 在加载期拦截，此处只做防御性截断。
func (inv *Inventory) ViaChain(host string) []string {
	var chain []string
	cur := host
	seen := map[string]bool{host: true}
	for {
		via := inv.ViaOf(cur)
		if via == "" || seen[via] {
			return chain
		}
		chain = append(chain, via)
		seen[via] = true
		cur = via
	}
}

// ViaRoot 返回中继链的根（无中继返回主机自身）。
func (inv *Inventory) ViaRoot(host string) string {
	chain := inv.ViaChain(host)
	if len(chain) == 0 {
		return host
	}
	return chain[len(chain)-1]
}

// ValidateVia 校验全部 via 声明：目标必须是清单内主机，链不得成环
// （环状可达是清单笔误，静默接受会让提交端在选择中继时死循环）。
func (inv *Inventory) ValidateVia() error {
	for _, h := range inv.Hosts {
		via := inv.ViaOf(h.Name)
		if via == "" {
			continue
		}
		// 逐跳走查：未知名/环都定位到具体路径
		path := []string{h.Name}
		seen := map[string]bool{h.Name: true}
		cur := h.Name
		for {
			next := inv.ViaOf(cur)
			if next == "" {
				break
			}
			if inv.HostByName(next) == nil {
				return fmt.Errorf("via target %q (from host %s) is not in the inventory", next, cur)
			}
			if seen[next] {
				return fmt.Errorf("via cycle detected: %s -> %s", strings.Join(path, " -> "), next)
			}
			path = append(path, next)
			seen[next] = true
			cur = next
		}
	}
	return nil
}
