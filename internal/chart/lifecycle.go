package chart

import (
	"fmt"
	"strings"

	"wdp/internal/model"
)

// PhasePlays 返回指定生命周期相位对应的 play 清单（deploy | uninstall | status；空串按 deploy）。
// uninstall/status 为可选清单，缺失时返回错误。
func (c *Chart) PhasePlays(phase string) ([]*model.Play, error) {
	switch phase {
	case "", "deploy":
		return c.Deploy, nil
	case "uninstall":
		if c.Uninstall == nil {
			return nil, fmt.Errorf("chart %s does not provide uninstall.yaml and cannot be uninstalled", c.Meta.Name)
		}
		return c.Uninstall, nil
	case "status":
		if c.Status == nil {
			return nil, fmt.Errorf("chart %s does not provide status.yaml", c.Meta.Name)
		}
		return c.Status, nil
	default:
		return nil, fmt.Errorf("unknown --phase %q (options: deploy/uninstall/status)", phase)
	}
}

// ValidateRequired 校验合并后的 values 覆盖 chart.yaml required 声明的全部点路径。
func (c *Chart) ValidateRequired(values map[string]any) error {
	if len(c.Meta.Required) == 0 {
		return nil
	}
	var missing []string
	for _, path := range c.Meta.Required {
		if !pathExists(values, path) {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config items (chart.yaml required): %s (provide via -f envs/*.yaml or --set)",
			strings.Join(missing, ", "))
	}
	return nil
}

// pathExists 按点路径检查 values 中是否存在该键（解析复用 --set 的
// parsePath，支持下标写法 a.b[0]——此前 "b[0]" 整段当键查找会误报缺失）。
func pathExists(values map[string]any, path string) bool {
	segs, err := parsePath(path)
	if err != nil || len(segs) == 0 {
		return false
	}
	cur := values
	for i, s := range segs {
		var v any
		if !s.hasID {
			c, ok := cur[s.key]
			if !ok || c == nil {
				return false
			}
			v = c
		} else {
			list, ok := cur[s.key].([]any)
			if !ok || s.idx >= len(list) || list[s.idx] == nil {
				return false
			}
			v = list[s.idx]
		}
		if i == len(segs)-1 {
			return true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return false
}
