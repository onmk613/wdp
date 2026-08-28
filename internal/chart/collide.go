package chart

// values / inventory 同名变量碰撞的分类规则（chart 语义域）。
//
// 语义：默认同名时 chart values 恒赢（chart 是产品、values 是参数面，
// inventory 变量是环境事实）。碰撞分两类：
//   - shadowed：inventory 变量被 values 静默遮蔽 → 命令层告警 + 修复提示
//     （改用独立键名 + 模板 dig，或把键列入 chart.yaml inventory_override）
//   - overridden：键在 inventory_override 白名单里，inventory 同名变量
//     反超 values → 命令层打印一条生效信息（显式 opt-in，不是问题）

import (
	"slices"

	"wdp/internal/model"
)

// builtinHostVarNames 是执行器强制注入的标识键，不属于站点配置，不参与碰撞判定。
var builtinHostVarNames = map[string]bool{
	"inventory_hostname": true,
	"group_names":        true,
}

// ValueCollisions 分类 values 顶层键与主机变量的同名碰撞，返回 key→主机
// 列表（shadowed / overridden，见包注释）。只看传入的主机：调用方负责
// 传入本次运行实际选中的主机，避免对 inventory 里无关主机误报。
func ValueCollisions(values map[string]any, hosts []*model.Host, allow []string) (shadowed, overridden map[string][]string) {
	if len(values) == 0 || len(hosts) == 0 {
		return nil, nil
	}
	allowSet := map[string]bool{}
	for _, k := range allow {
		allowSet[k] = true
	}
	shadowed, overridden = map[string][]string{}, map[string][]string{}
	for _, h := range hosts {
		for k := range h.Vars {
			if builtinHostVarNames[k] {
				continue
			}
			if _, isValuesKey := values[k]; !isValuesKey {
				continue
			}
			target := shadowed
			if allowSet[k] {
				target = overridden
			}
			if !slices.Contains(target[k], h.Name) {
				target[k] = append(target[k], h.Name)
			}
		}
	}
	return shadowed, overridden
}
