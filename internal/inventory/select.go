package inventory

import (
	"fmt"
	"path"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/model"
)

// Select 按模式选择主机。表达式语法：
//
//	逗号联合：webservers,dbservers；! 前缀排除：all,!web1
//	:& 交集：webservers:&production（可链式 a:&b:&c）
//	通配：web*（同时匹配主机名与组名，组名展开为成员；? 单字符）
func (inv *Inventory) Select(pattern string) ([]*model.Host, error) {
	include := map[string]bool{}
	exclude := map[string]bool{}
	for _, token := range strings.Split(pattern, ",") {
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
				return nil, fmt.Errorf(i18n.T("host pattern %q contains an empty segment", "主机模式 %q 存在空片段"), token)
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
		for name, g := range inv.Groups {
			if ok, _ := path.Match(seg, name); ok {
				matched = true
				for _, h := range expandGroup(inv, g, map[string]bool{}) {
					out[h] = true
				}
			}
		}
		for _, h := range inv.Hosts {
			if ok, _ := path.Match(seg, h.Name); ok {
				matched = true
				out[h.Name] = true
			}
		}
		if !matched {
			return nil, fmt.Errorf(i18n.T("no host or group matches %q", "未找到匹配 %q 的主机或组"), seg)
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
		return nil, fmt.Errorf(i18n.T("host or group not found: %s", "未找到主机或组: %s"), seg)
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
