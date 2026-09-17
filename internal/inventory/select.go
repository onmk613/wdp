package inventory

import (
	"fmt"
	"path"
	"strings"

	"wdp/internal/model"
)

// Select 按模式选择主机。表达式语法：
// webservers,dbservers            # 两组并集
// all,!web1                       # 除 web1 外全部
// webservers:&production          # 交集：生产环境的 web
// webservers:&production,!canary  # 生产 web 且非金丝雀（docs/03 的例子）
// web*                            # 所有 web 开头的组（展开成员）+ 主机
// db?                             # db1、dba……单字符
// os_[12]                         # os_1、os_2 字符类
// prod_*:&appservers              # 通配与交集链混用
func (inv *Inventory) Select(pattern string) ([]*model.Host, error) {
	include := map[string]bool{}
	exclude := map[string]bool{}
	for token := range strings.SplitSeq(pattern, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		neg := strings.HasPrefix(token, "!")
		if neg {
			token = strings.TrimSpace(token[1:])
		}
		// :& 交集链
		set := map[string]bool{}
		for i, seg := range strings.Split(token, ":&") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				return nil, fmt.Errorf("host pattern %q contains an empty segment", token)
			}
			matched, err := inv.matchSegment(seg)
			if err != nil {
				return nil, err
			}
			if i == 0 {
				set = matched
				continue
			}
			for n := range set {
				if !matched[n] {
					delete(set, n)
				}
			}
		}
		target := include
		if neg {
			target = exclude
		}
		for n := range set {
			target[n] = true
		}
	}

	var out []*model.Host
	for _, h := range inv.Hosts {
		if include[h.Name] && !exclude[h.Name] {
			out = append(out, h)
		}
	}
	return out, nil
}

// SelectLimited 按模式选择主机并应用 --limit 收窄（limit 是对已选范围的
// 进一步过滤，与 executor 的选择口径一致；limit 模式非法时报错）。
func (inv *Inventory) SelectLimited(pattern, limit string) ([]*model.Host, error) {
	hosts, err := inv.Select(pattern)
	if err != nil {
		return nil, err
	}
	return applyLimit(inv, hosts, limit)
}

// SelectPlays 取 plays 全部 hosts 模式的并集（同名主机去重），再应用
// --limit 收窄——与 executor 实际执行的主机范围同口径。模式解析失败的
// play 跳过（调用方此前已让 executor 校验过全部模式）。
// 返回按 inventory 声明序排列（去重集落 map 后按 inv.Hosts 顺序回排，
// 不随 map 迭代漂移——plan 编译的逐字节确定性依赖此性质）。
func (inv *Inventory) SelectPlays(plays []*model.Play, limit string) []*model.Host {
	set := map[string]*model.Host{}
	for _, p := range plays {
		hosts, err := inv.Select(p.Hosts)
		if err != nil {
			continue
		}
		for _, h := range hosts {
			set[h.Name] = h
		}
	}
	out := make([]*model.Host, 0, len(set))
	for _, h := range inv.Hosts {
		if set[h.Name] != nil {
			out = append(out, h)
			delete(set, h.Name)
		}
	}
	// 兜底：不在 inv.Hosts 的主机（理论上不存在——Select 只产出清单主机）
	for _, h := range set {
		out = append(out, h)
	}
	filtered, _ := applyLimit(inv, out, limit)
	return filtered
}

// applyLimit 用 --limit 模式收窄主机列表。
func applyLimit(inv *Inventory, hosts []*model.Host, limit string) ([]*model.Host, error) {
	if limit == "" {
		return hosts, nil
	}
	limited, err := inv.Select(limit)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, h := range limited {
		keep[h.Name] = true
	}
	filtered := hosts[:0]
	for _, h := range hosts {
		if keep[h.Name] {
			filtered = append(filtered, h)
		}
	}
	return filtered, nil
}

// matchSegment 解析单个选择片段：all / * / 组名 / 主机名 / 通配模式。
func (inv *Inventory) matchSegment(seg string) (map[string]bool, error) {
	out := map[string]bool{}
	switch {
	case seg == "all" || seg == "*":
		for _, h := range inv.Hosts {
			out[h.Name] = true
		}
		return out, nil
	case strings.ContainsAny(seg, "*?["):
		matched := false
		// 非法通配模式（如未闭合的 '['）必须报模式错误，而不是静默当成
		// "没有匹配"——`wdp inventory '['` 此前给出的是误导性的空结果
		for name, g := range inv.Groups {
			ok, err := path.Match(seg, name)
			if err != nil {
				return nil, fmt.Errorf("invalid host pattern %q: %w", seg, err)
			}
			if ok {
				matched = true
				for _, h := range expandGroup(inv, g, map[string]bool{}) {
					out[h] = true
				}
			}
		}
		for _, h := range inv.Hosts {
			ok, err := path.Match(seg, h.Name)
			if err != nil {
				return nil, fmt.Errorf("invalid host pattern %q: %w", seg, err)
			}
			if ok {
				matched = true
				out[h.Name] = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("no host or group matches %q", seg)
		}
		return out, nil
	default:
		if g, ok := inv.Groups[seg]; ok {
			for _, h := range expandGroup(inv, g, map[string]bool{}) {
				out[h] = true
			}
			return out, nil
		}
		if inv.hostExists(seg) {
			out[seg] = true
			return out, nil
		}
		return nil, fmt.Errorf("host or group not found: %s", seg)
	}
}

func (inv *Inventory) hostExists(name string) bool {
	for _, h := range inv.Hosts {
		if h.Name == name {
			return true
		}
	}
	return false
}
